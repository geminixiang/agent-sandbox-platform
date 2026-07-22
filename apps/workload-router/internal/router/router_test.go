package router

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geminixiang/agent-sandbox-platform/apps/workload-router/internal/auth"
	"github.com/geminixiang/agent-sandbox-platform/apps/workload-router/internal/resolver"
)

const testHost = "opaque.router.test"

type fixedResolver struct {
	target resolver.HTTPServiceTarget
	err    error
}

func (r fixedResolver) Resolve(context.Context, string) (resolver.HTTPServiceTarget, error) {
	return r.target, r.err
}

type fixture struct {
	registry   *auth.Registry
	handler    *Handler
	server     *httptest.Server
	upstream   *httptest.Server
	capability string
	target     resolver.HTTPServiceTarget
}

func newFixture(t *testing.T, upstream http.Handler, allowWebSocket bool, maxConnections int) *fixture {
	t.Helper()
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	upstreamURL, _ := url.Parse(upstreamServer.URL)
	_, portText, _ := net.SplitHostPort(upstreamURL.Host)
	var port uint16
	_, _ = fmt.Sscan(portText, &port)
	target, err := resolver.NewHTTPServiceTarget("fixture", "sandbox", port)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: time.Second}
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", upstreamURL.Host)
	}
	registry := auth.NewRegistry(time.Hour, nil)
	t.Cleanup(registry.Close)
	capability, err := registry.Register(auth.Registration{
		ExposureID: "exp-1", Host: testHost, ExpiresAt: time.Now().Add(time.Minute),
		AllowWebSocket: allowWebSocket, MaxConnections: maxConnections,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(registry, fixedResolver{target: target}, Options{Transport: transport, MaxRequestBodyBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &fixture{registry: registry, handler: handler, server: server, upstream: upstreamServer, capability: capability, target: target}
}

func (f *fixture) exchange(t *testing.T, capability string) *http.Cookie {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, f.server.URL+BootstrapEndpoint(), strings.NewReader(capability))
	request.Host = testHost
	request.Header.Set("Content-Type", "text/plain; charset=UTF-8")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("exchange status = %d, body = %q", response.StatusCode, body)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %#v", cookies)
	}
	return cookies[0]
}

func (f *fixture) request(t *testing.T, method, path string, body io.Reader, cookie *http.Cookie) *http.Response {
	t.Helper()
	request, _ := http.NewRequest(method, f.server.URL+path, body)
	request.Host = testHost
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestBootstrapDocumentAndCookieSecurity(t *testing.T) {
	f := newFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), false, 4)
	request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/anything", nil)
	request.Host = testHost
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != BootstrapDocument() {
		t.Fatal("bootstrap response was not the fixed document")
	}
	csp := response.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'self'") || !strings.Contains(csp, "sha256-") {
		t.Fatalf("CSP = %q", csp)
	}
	if strings.Contains(string(body), f.capability) {
		t.Fatal("bootstrap document contained capability")
	}
	response = f.request(t, http.MethodPost, "/workload", nil, nil)
	unauthorizedBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || strings.Contains(string(unauthorizedBody), "<!doctype") {
		t.Fatalf("unauthenticated workload POST status/body = %d %q", response.StatusCode, unauthorizedBody)
	}
	cookie := f.exchange(t, f.capability)
	if cookie.Name != SessionCookieName || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("unsafe cookie = %#v", cookie)
	}
	if cookie.Expires.After(time.Now().Add(time.Minute+time.Second)) || cookie.MaxAge <= 0 {
		t.Fatalf("unbounded cookie = %#v", cookie)
	}
	response = f.request(t, http.MethodPost, BootstrapEndpoint(), strings.NewReader(f.capability), nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status = %d", response.StatusCode)
	}
}

func TestHTTPProxyPreservesSemanticsAndStripsCredentials(t *testing.T) {
	type seenRequest struct {
		Path               string
		RawQuery           string
		Authorization      string
		ProxyAuthorization string
		Internal           string
		Cookie             string
		Body               string
	}
	seen := make(chan seenRequest, 1)
	upstream := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		seen <- seenRequest{request.URL.Path, request.URL.RawQuery, request.Header.Get("Authorization"), request.Header.Get("Proxy-Authorization"), request.Header.Get("X-ASP-Exposure-ID"), request.Header.Get("Cookie"), string(body)}
		response.Header().Add("Set-Cookie", SessionCookieName+"=replace; Secure; Path=/")
		response.Header().Add("Set-Cookie", "workload=value; Path=/")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte("upstream body"))
	})
	f := newFixture(t, upstream, false, 4)
	cookie := f.exchange(t, f.capability)
	request, _ := http.NewRequest(http.MethodPost, f.server.URL+"/assets/app.js?x=1&x=2", strings.NewReader("payload"))
	request.Host = testHost
	request.AddCookie(cookie)
	request.AddCookie(&http.Cookie{Name: "workload-in", Value: "ok"})
	request.Header.Set("Authorization", "Bearer platform-secret")
	request.Header.Set("Proxy-Authorization", "Bearer proxy-secret")
	request.Header.Set("X-ASP-Exposure-ID", "spoofed")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || string(body) != "upstream body" {
		t.Fatalf("proxy response = %d %q", response.StatusCode, body)
	}
	got := <-seen
	if got.Path != "/assets/app.js" || got.RawQuery != "x=1&x=2" || got.Body != "payload" {
		t.Fatalf("upstream request = %#v", got)
	}
	if got.Authorization != "" || got.ProxyAuthorization != "" || got.Internal != "" || strings.Contains(got.Cookie, SessionCookieName) || !strings.Contains(got.Cookie, "workload-in=ok") {
		t.Fatalf("credential stripping failed: %#v", got)
	}
	setCookies := response.Header.Values("Set-Cookie")
	if len(setCookies) != 1 || !strings.HasPrefix(setCookies[0], "workload=") {
		t.Fatalf("response cookies = %#v", setCookies)
	}
}

func TestRedirectRewritesOnlyTheUpstreamService(t *testing.T) {
	var authority string
	upstream := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Location", "http://"+authority+"/next?q=1#part")
		response.WriteHeader(http.StatusTemporaryRedirect)
	})
	f := newFixture(t, upstream, false, 4)
	authority = f.target.Authority()
	cookie := f.exchange(t, f.capability)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/start", nil)
	request.Host = testHost
	request.AddCookie(cookie)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got := response.Header.Get("Location"); got != "https://"+testHost+"/next?q=1#part" {
		t.Fatalf("Location = %q", got)
	}
}

func TestRedirectsFailClosedOnAmbiguousOrInternalLocation(t *testing.T) {
	tests := []struct {
		name      string
		locations []string
	}{
		{name: "multiple with second internal leak", locations: []string{"https://example.test/ok", "http://other.sandbox.svc/private"}},
		{name: "service suffix", locations: []string{"http://other.sandbox.svc/private"}},
		{name: "cluster service suffix", locations: []string{"http://other.sandbox.svc.cluster.local/private"}},
		{name: "malformed", locations: []string{"http://[::1"}},
		{name: "userinfo", locations: []string{"https://user@example.test/private"}},
		{name: "unsafe scheme", locations: []string{"javascript:alert(1)"}},
		{name: "scheme without authority", locations: []string{"https:/private"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				for _, location := range test.locations {
					response.Header().Add("Location", location)
				}
				response.WriteHeader(http.StatusFound)
			})
			f := newFixture(t, upstream, false, 2)
			cookie := f.exchange(t, f.capability)
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/redirect", nil)
			request.Host = testHost
			request.AddCookie(cookie)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d", response.StatusCode)
			}
			if leaked := response.Header.Values("Location"); len(leaked) != 0 {
				t.Fatalf("Location leaked on failed redirect: %#v", leaked)
			}
		})
	}
}

func TestClearSiteDataCannotClearRouterSession(t *testing.T) {
	tests := []struct {
		name      string
		values    []string
		wantStrip bool
	}{
		{name: "cookies case insensitive in second header", values: []string{`"cache"`, `"CoOkIeS"`}, wantStrip: true},
		{name: "wildcard among directives", values: []string{`"cache", "*"`}, wantStrip: true},
		{name: "unquoted words are not directives", values: []string{"cookies, *"}},
		{name: "unrelated directives remain", values: []string{`"cache", "storage"`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := make(http.Header)
			for _, value := range test.values {
				header.Add("Clear-Site-Data", value)
			}
			stripReservedResponseHeaders(header)
			if got := len(header.Values("Clear-Site-Data")) == 0; got != test.wantStrip {
				t.Fatalf("stripped = %v, headers = %#v", got, header.Values("Clear-Site-Data"))
			}
		})
	}
}

func TestUnknownAmbiguousHostsAndInvalidResolverAreDenied(t *testing.T) {
	var upstreamReached atomic.Bool
	f := newFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamReached.Store(true) }), false, 4)
	for _, host := range []string{"unknown.router.test", testHost + ":443", testHost + ".", "127.0.0.1"} {
		request, _ := http.NewRequest(http.MethodGet, f.server.URL+"/", nil)
		request.Host = host
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("host %q status = %d", host, response.StatusCode)
		}
	}
	if upstreamReached.Load() {
		t.Fatal("an invalid Host reached the upstream")
	}

	registry := auth.NewRegistry(0, nil)
	defer registry.Close()
	capability, _ := registry.Register(auth.Registration{ExposureID: "bad", Host: testHost, ExpiresAt: time.Now().Add(time.Minute), MaxConnections: 1})
	handler, _ := New(registry, fixedResolver{}, Options{})
	session, _, _ := registry.Exchange(testHost, capability)
	request := httptest.NewRequest(http.MethodGet, "https://"+testHost+"/", nil)
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: session})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("zero target status = %d", recorder.Code)
	}
}

func TestBodyAndConnectionLimits(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	upstream := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = response.Write([]byte("ok"))
	})
	f := newFixture(t, upstream, false, 1)
	cookie := f.exchange(t, f.capability)
	response := f.request(t, http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 1025)), cookie)
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("large body status = %d", response.StatusCode)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		response := f.request(t, http.MethodGet, "/first", nil, cookie)
		response.Body.Close()
	}()
	<-entered
	response = f.request(t, http.MethodGet, "/second", nil, cookie)
	response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Errorf("connection limit status = %d", response.StatusCode)
	}
	close(release)
	wg.Wait()
}

func TestNewHTTPServerBoundsSlowHeadersAndBodies(t *testing.T) {
	t.Run("slow headers", func(t *testing.T) {
		f := newFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), false, 2)
		address, _ := startHTTPServer(t, f.handler, func(server *http.Server) {
			if server.ReadHeaderTimeout <= 0 || server.MaxHeaderBytes <= 0 {
				t.Fatal("NewHTTPServer omitted header bounds")
			}
			server.ReadHeaderTimeout = 75 * time.Millisecond
		})
		connection, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		_, _ = io.WriteString(connection, "GET / HTTP/1.1\r\nHost: ")
		started := time.Now()
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		_, err = connection.Read(make([]byte, 1))
		assertBoundedRead(t, started, err)
	})

	t.Run("slow bootstrap body", func(t *testing.T) {
		f := newFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), false, 2)
		f.handler.bodyTimeout = 75 * time.Millisecond
		address, _ := startHTTPServer(t, f.handler, nil)
		connection, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		fmt.Fprintf(connection, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: text/plain\r\nContent-Length: 43\r\nConnection: close\r\n\r\nx", BootstrapEndpoint(), testHost)
		started := time.Now()
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
		assertBoundedRead(t, started, err)
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d", response.StatusCode)
			}
		}
	})

	t.Run("slow proxied body", func(t *testing.T) {
		upstream := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			if _, err := io.Copy(io.Discard, request.Body); err == nil {
				_, _ = response.Write([]byte("unexpected"))
			}
		})
		f := newFixture(t, upstream, false, 2)
		f.handler.bodyTimeout = 75 * time.Millisecond
		session, _, err := f.registry.Exchange(testHost, f.capability)
		if err != nil {
			t.Fatal(err)
		}
		address, _ := startHTTPServer(t, f.handler, nil)
		connection, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		fmt.Fprintf(connection, "POST /slow HTTP/1.1\r\nHost: %s\r\nCookie: %s=%s\r\nContent-Length: 4\r\nConnection: close\r\n\r\nx", testHost, SessionCookieName, session)
		started := time.Now()
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		_, err = http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
		assertBoundedRead(t, started, err)
	})
}

func TestNewHTTPServerWebSocketSurvivesBodyDeadline(t *testing.T) {
	f := newFixture(t, http.HandlerFunc(websocketEcho), true, 2)
	f.handler.bodyTimeout = 50 * time.Millisecond
	session, _, err := f.registry.Exchange(testHost, f.capability)
	if err != nil {
		t.Fatal(err)
	}
	address, server := startHTTPServer(t, f.handler, nil)
	if server.WriteTimeout != 0 {
		t.Fatalf("absolute WriteTimeout would break streaming: %v", server.WriteTimeout)
	}
	cookie := &http.Cookie{Name: SessionCookieName, Value: session}
	connection, reader := openWebSocket(t, address, cookie)
	defer connection.Close()
	time.Sleep(3 * f.handler.bodyTimeout)
	payload := []byte("still-open")
	if _, err := connection.Write(maskedTextFrame(payload)); err != nil {
		t.Fatal(err)
	}
	opcode, got := readFrame(t, reader)
	if opcode != 1 || string(got) != string(payload) {
		t.Fatalf("echo after body deadline = opcode %d payload %q", opcode, got)
	}
}

func startHTTPServer(t *testing.T, handler http.Handler, configure func(*http.Server)) (string, *http.Server) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := NewHTTPServer(listener.Addr().String(), handler)
	if configure != nil {
		configure(server)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String(), server
}

func assertBoundedRead(t *testing.T, started time.Time, err error) {
	t.Helper()
	elapsed := time.Since(started)
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("server did not enforce its deadline before client timeout (%v)", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("bounded read took %v", elapsed)
	}
}

func TestRevokeClosesStreamingHTTP(t *testing.T) {
	upstream := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("first"))
		response.(http.Flusher).Flush()
		<-request.Context().Done()
	})
	f := newFixture(t, upstream, false, 2)
	cookie := f.exchange(t, f.capability)
	response := f.request(t, http.MethodGet, "/stream", nil, cookie)
	defer response.Body.Close()
	first := make([]byte, 5)
	if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "first" {
		t.Fatalf("initial stream read = %q, %v", first, err)
	}

	started := time.Now()
	f.registry.Revoke("exp-1")
	closed := make(chan error, 1)
	go func() {
		_, err := response.Body.Read(make([]byte, 1))
		closed <- err
	}()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("stream produced data after revoke")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("stream revoke took %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("stream remained open one second after revoke")
	}
}

func TestWebSocketOriginMustBeUniqueStrictHTTPSSameOrigin(t *testing.T) {
	tests := []struct {
		name    string
		origins []string
	}{
		{name: "absent"},
		{name: "duplicate", origins: []string{"https://" + testHost, "https://" + testHost}},
		{name: "http", origins: []string{"http://" + testHost}},
		{name: "sibling", origins: []string{"https://sibling.router.test"}},
		{name: "userinfo", origins: []string{"https://user@" + testHost}},
		{name: "slash path", origins: []string{"https://" + testHost + "/"}},
		{name: "path", origins: []string{"https://" + testHost + "/path"}},
		{name: "query", origins: []string{"https://" + testHost + "?x=1"}},
		{name: "fragment", origins: []string{"https://" + testHost + "#fragment"}},
		{name: "malformed", origins: []string{"://" + testHost}},
	}
	f := newFixture(t, http.HandlerFunc(websocketEcho), true, 2)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://"+testHost+"/ws", nil)
			request.Header.Set("Connection", "Upgrade")
			request.Header.Set("Upgrade", "websocket")
			for _, origin := range test.origins {
				request.Header.Add("Origin", origin)
			}
			recorder := httptest.NewRecorder()
			f.handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d", recorder.Code)
			}
		})
	}
}

func TestWebSocketEchoAndRevokeClosesConnection(t *testing.T) {
	upstream := http.HandlerFunc(websocketEcho)
	f := newFixture(t, upstream, true, 2)
	cookie := f.exchange(t, f.capability)
	connection, reader := openWebSocket(t, f.server.Listener.Addr().String(), cookie)
	defer connection.Close()

	payload := []byte("hello")
	if _, err := connection.Write(maskedTextFrame(payload)); err != nil {
		t.Fatal(err)
	}
	opcode, got := readFrame(t, reader)
	if opcode != 1 || string(got) != string(payload) {
		t.Fatalf("echo frame = opcode %d payload %q", opcode, got)
	}

	f.registry.Revoke("exp-1")
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("websocket remained open after revoke")
	}
}

func TestWebSocketPolicyAndPreBootstrapDenial(t *testing.T) {
	f := newFixture(t, http.HandlerFunc(websocketEcho), false, 2)
	connection, err := net.Dial("tcp", f.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(connection, "GET /ws HTTP/1.1\r\nHost: %s\r\nOrigin: https://%s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", testHost, testHost)
	response, _ := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodGet})
	connection.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pre-bootstrap websocket status = %d", response.StatusCode)
	}
	response.Body.Close()

	cookie := f.exchange(t, f.capability)
	connection, err = net.Dial("tcp", f.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(connection, "GET /ws HTTP/1.1\r\nHost: %s\r\nOrigin: https://%s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nCookie: %s=%s\r\n\r\n", testHost, testHost, cookie.Name, cookie.Value)
	response, _ = http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodGet})
	connection.Close()
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("policy websocket status = %d", response.StatusCode)
	}
}

func openWebSocket(t *testing.T, address string, cookie *http.Cookie) (net.Conn, *bufio.Reader) {
	t.Helper()
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	fmt.Fprintf(connection, "GET /ws?channel=1 HTTP/1.1\r\nHost: %s\r\nOrigin: https://%s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nCookie: %s=%s\r\n\r\n", testHost, testHost, key, cookie.Name, cookie.Value)
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("websocket status = %d body = %q", response.StatusCode, body)
	}
	return connection, reader
}

func websocketEcho(response http.ResponseWriter, request *http.Request) {
	hijacker, ok := response.(http.Hijacker)
	if !ok {
		http.Error(response, "unsupported", http.StatusInternalServerError)
		return
	}
	connection, buffer, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer connection.Close()
	acceptDigest := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	fmt.Fprintf(buffer, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(acceptDigest[:]))
	_ = buffer.Flush()
	reader := bufio.NewReader(connection)
	opcode, payload, err := readRawFrame(reader)
	if err != nil {
		return
	}
	_, _ = connection.Write(append([]byte{0x80 | opcode, byte(len(payload))}, payload...))
	_, _ = io.Copy(io.Discard, connection)
}

func maskedTextFrame(payload []byte) []byte {
	mask := [4]byte{1, 2, 3, 4}
	frame := []byte{0x81, 0x80 | byte(len(payload)), mask[0], mask[1], mask[2], mask[3]}
	for index, value := range payload {
		frame = append(frame, value^mask[index%4])
	}
	return frame
}

func readFrame(t *testing.T, reader *bufio.Reader) (byte, []byte) {
	t.Helper()
	opcode, payload, err := readRawFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	return opcode, payload
}

func readRawFrame(reader *bufio.Reader) (byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, nil, err
	}
	length := int(header[1] & 0x7f)
	if length >= 126 {
		return 0, nil, fmt.Errorf("test frame too large")
	}
	var mask []byte
	if header[1]&0x80 != 0 {
		mask = make([]byte, 4)
		if _, err := io.ReadFull(reader, mask); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	for index := range payload {
		if mask != nil {
			payload[index] ^= mask[index%4]
		}
	}
	return header[0] & 0xf, payload, nil
}

func TestSecretSafeEvents(t *testing.T) {
	var events []auth.Event
	registry := auth.NewRegistry(0, func(event auth.Event) { events = append(events, event) })
	defer registry.Close()
	capability, _ := registry.Register(auth.Registration{ExposureID: "opaque-id", Host: testHost, ExpiresAt: time.Now().Add(time.Minute), MaxConnections: 1})
	_, _, _ = registry.Exchange(testHost, capability)
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), capability) || strings.Contains(string(encoded), testHost) {
		t.Fatalf("events leaked secret or host: %s", encoded)
	}
}
