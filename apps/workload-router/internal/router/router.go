// Package router implements the capability-authenticated workload data plane.
package router

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/geminixiang/agent-sandbox-platform/apps/workload-router/internal/auth"
	"github.com/geminixiang/agent-sandbox-platform/apps/workload-router/internal/resolver"
)

const (
	SessionCookieName  = "__Host-asp_session"
	bootstrapPath      = "/_asp/bootstrap"
	defaultBodyLimit   = int64(16 << 20)
	defaultBodyTimeout = 10 * time.Second
)

const bootstrapScript = `(()=>{const p=new URLSearchParams(location.hash.slice(1));const c=p.get("asp");if(!c){document.body.textContent="Authorization required";return}fetch("/_asp/bootstrap",{method:"POST",headers:{"Content-Type":"text/plain"},body:c,credentials:"same-origin",cache:"no-store",redirect:"error"}).then(r=>{if(!r.ok)throw new Error("denied");location.replace("/")}).catch(()=>{document.body.textContent="Authorization failed"})})();`

const bootstrapHTML = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Opening workload</title></head><body>Authorizing…<script>` + bootstrapScript + `</script></body></html>`

type contextKey struct{}

// Options controls prototype resource bounds. Server-level header and timeout
// bounds are provided by NewHTTPServer below.
type Options struct {
	MaxRequestBodyBytes int64
	BodyReadTimeout     time.Duration
	Transport           *http.Transport
}

// Handler has only three collaborators: authorization state, the constrained
// resolver seam, and the standard reverse proxy transport.
type Handler struct {
	registry    *auth.Registry
	resolver    resolver.Resolver
	transport   *http.Transport
	bodyLimit   int64
	bodyTimeout time.Duration
	csp         string
}

func New(registry *auth.Registry, targetResolver resolver.Resolver, options Options) (*Handler, error) {
	if registry == nil || targetResolver == nil {
		return nil, fmt.Errorf("registry and resolver are required")
	}
	bodyLimit := options.MaxRequestBodyBytes
	if bodyLimit <= 0 {
		bodyLimit = defaultBodyLimit
	}

	bodyTimeout := options.BodyReadTimeout
	if bodyTimeout <= 0 {
		bodyTimeout = defaultBodyTimeout
	}

	transport := options.Transport
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport)
	}
	transport = transport.Clone()
	baseDial := transport.DialContext
	if baseDial == nil {
		dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		baseDial = dialer.DialContext
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := baseDial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		exposure, ok := ctx.Value(contextKey{}).(auth.Exposure)
		if !ok || exposure.ID == "" {
			_ = conn.Close()
			return nil, fmt.Errorf("missing trusted exposure context")
		}
		return registry.TrackConnection(exposure, conn)
	}
	// Pooling by Service authority could let two Exposures share a connection,
	// making exposure-scoped revocation unable to identify its owner. The
	// single-replica prototype therefore trades efficiency for exact tracking.
	transport.DisableKeepAlives = true
	if transport.ResponseHeaderTimeout == 0 {
		transport.ResponseHeaderTimeout = 15 * time.Second
	}
	if transport.IdleConnTimeout == 0 {
		transport.IdleConnTimeout = 60 * time.Second
	}
	if transport.TLSHandshakeTimeout == 0 {
		transport.TLSHandshakeTimeout = 5 * time.Second
	}

	digest := sha256.Sum256([]byte(bootstrapScript))
	csp := "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(digest[:]) + "'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	return &Handler{registry: registry, resolver: targetResolver, transport: transport, bodyLimit: bodyLimit, bodyTimeout: bodyTimeout, csp: csp}, nil
}

// NewHTTPServer applies bounds that an http.Handler cannot enforce. TLS is
// intentionally external/operator-owned; this server receives trusted
// cleartext traffic from the TLS terminator.
func NewHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}

func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	host, err := auth.CanonicalHost(request.Host)
	if err != nil {
		writePlain(response, http.StatusNotFound, "not found")
		return
	}
	if _, err := h.registry.LookupHost(host); err != nil {
		writePlain(response, http.StatusNotFound, "not found")
		return
	}

	websocket := isWebSocket(request)
	if websocket && !validWebSocketOrigin(request, host) {
		writePlain(response, http.StatusForbidden, "invalid websocket origin")
		return
	}
	if !websocket && request.Body != nil && request.Body != http.NoBody {
		clearDeadline, err := h.setBodyReadDeadline(response, request)
		if err != nil {
			writePlain(response, http.StatusServiceUnavailable, "request body unavailable")
			return
		}
		defer clearDeadline()
	}

	if request.URL.Path == bootstrapPath {
		if request.Method != http.MethodPost {
			writePlain(response, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.exchange(response, request, host)
		return
	}

	session, ok := sessionCredential(request)
	if !ok {
		if websocket {
			writePlain(response, http.StatusUnauthorized, "authorization required")
			return
		}
		if request.Method == http.MethodGet || request.Method == http.MethodHead {
			h.bootstrap(response, request)
			return
		}
		writePlain(response, http.StatusUnauthorized, "authorization required")
		return
	}
	exposure, err := h.registry.Authenticate(host, session)
	if err != nil {
		if websocket || (request.Method != http.MethodGet && request.Method != http.MethodHead) {
			writePlain(response, http.StatusUnauthorized, "authorization required")
			return
		}
		h.bootstrap(response, request)
		return
	}
	if websocket && !exposure.AllowWebSocket {
		writePlain(response, http.StatusForbidden, "websocket disabled")
		return
	}
	if request.ContentLength > h.bodyLimit {
		writePlain(response, http.StatusRequestEntityTooLarge, "request too large")
		return
	}
	release, err := h.registry.Acquire(exposure)
	if err != nil {
		if errors.Is(err, auth.ErrConnectionLimit) {
			writePlain(response, http.StatusTooManyRequests, "connection limit reached")
		} else {
			writePlain(response, http.StatusUnauthorized, "authorization required")
		}
		return
	}
	defer release()

	target, err := h.resolver.Resolve(request.Context(), exposure.ID)
	if err != nil || target.Validate() != nil {
		writePlain(response, http.StatusBadGateway, "upstream unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, h.bodyLimit)
	h.proxy(response, request, exposure, target)
}

func (h *Handler) setBodyReadDeadline(response http.ResponseWriter, request *http.Request) (func(), error) {
	controller := http.NewResponseController(response)
	if err := controller.SetReadDeadline(time.Now().Add(h.bodyTimeout)); err != nil {
		return nil, err
	}
	var once sync.Once
	clear := func() {
		once.Do(func() { _ = controller.SetReadDeadline(time.Time{}) })
	}
	request.Body = &deadlineBody{ReadCloser: request.Body, clear: clear}
	return clear, nil
}

type deadlineBody struct {
	io.ReadCloser
	clear func()
}

func (b *deadlineBody) Read(buffer []byte) (int, error) {
	count, err := b.ReadCloser.Read(buffer)
	if errors.Is(err, io.EOF) {
		b.clear()
	}
	return count, err
}

func (b *deadlineBody) Close() error {
	err := b.ReadCloser.Close()
	b.clear()
	return err
}

func (h *Handler) bootstrap(response http.ResponseWriter, request *http.Request) {
	setSecurityHeaders(response.Header())
	response.Header().Set("Content-Security-Policy", h.csp)
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = io.WriteString(response, bootstrapHTML)
	}
}

func (h *Handler) exchange(response http.ResponseWriter, request *http.Request, host string) {
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || contentType != "text/plain" {
		writePlain(response, http.StatusUnauthorized, "authorization failed")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, 1024))
	if err != nil {
		writePlain(response, http.StatusUnauthorized, "authorization failed")
		return
	}
	session, expiresAt, err := h.registry.Exchange(host, string(body))
	if err != nil {
		writePlain(response, http.StatusUnauthorized, "authorization failed")
		return
	}
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(response, &http.Cookie{
		Name:     SessionCookieName,
		Value:    session,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	setSecurityHeaders(response.Header())
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(http.StatusNoContent)
}

func (h *Handler) proxy(response http.ResponseWriter, request *http.Request, exposure auth.Exposure, target resolver.HTTPServiceTarget) {
	proxy := &httputil.ReverseProxy{
		Transport:     h.transport,
		FlushInterval: -1,
		Rewrite: func(proxyRequest *httputil.ProxyRequest) {
			out := proxyRequest.Out
			out.URL.Scheme = "http"
			out.URL.Host = target.Authority()
			out.Host = target.Authority()
			stripSensitiveRequestHeaders(out.Header)
			stripSessionCookie(out.Header)
			*out = *out.WithContext(context.WithValue(out.Context(), contextKey{}, exposure))
		},
		ModifyResponse: func(upstream *http.Response) error {
			stripReservedResponseHeaders(upstream.Header)
			return rewriteLocation(upstream.Header, request.Host, target)
		},
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, _ error) {
			writePlain(writer, http.StatusBadGateway, "upstream unavailable")
		},
	}
	proxy.ServeHTTP(response, request)
}

func sessionCredential(request *http.Request) (string, bool) {
	var value string
	count := 0
	for _, cookie := range request.Cookies() {
		if cookie.Name == SessionCookieName {
			count++
			value = cookie.Value
		}
	}
	return value, count == 1
}

func stripSensitiveRequestHeaders(header http.Header) {
	for name := range header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "proxy-authorization" || strings.HasPrefix(lower, "x-asp-") {
			header.Del(name)
		}
	}
}

func stripSessionCookie(header http.Header) {
	values := header.Values("Cookie")
	header.Del("Cookie")
	var kept []string
	for _, value := range values {
		for _, pair := range strings.Split(value, ";") {
			pair = strings.TrimSpace(pair)
			name, _, found := strings.Cut(pair, "=")
			if found && name != SessionCookieName {
				kept = append(kept, pair)
			}
		}
	}
	if len(kept) != 0 {
		header.Set("Cookie", strings.Join(kept, "; "))
	}
}

func stripReservedResponseHeaders(header http.Header) {
	cookies := header.Values("Set-Cookie")
	header.Del("Set-Cookie")
	for _, cookie := range cookies {
		name, _, found := strings.Cut(cookie, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), SessionCookieName) {
			header.Add("Set-Cookie", cookie)
		}
	}
	for _, value := range header.Values("Clear-Site-Data") {
		lower := strings.ToLower(value)
		if strings.Contains(lower, `"cookies"`) || strings.Contains(lower, `"*"`) {
			header.Del("Clear-Site-Data")
			break
		}
	}
}

func rewriteLocation(header http.Header, externalHost string, target resolver.HTTPServiceTarget) error {
	locations := header.Values("Location")
	if len(locations) == 0 {
		return nil
	}
	if len(locations) != 1 || locations[0] == "" {
		return fmt.Errorf("ambiguous upstream redirect")
	}
	parsed, err := url.Parse(locations[0])
	if err != nil || parsed.User != nil || parsed.Opaque != "" {
		return fmt.Errorf("invalid upstream redirect")
	}
	if parsed.Scheme != "" && !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("invalid upstream redirect")
	}
	if parsed.Host == "" {
		if parsed.Scheme != "" {
			return fmt.Errorf("invalid upstream redirect")
		}
		return nil
	}
	if target.MatchesAuthority(parsed.Host) {
		parsed.Scheme = "https"
		parsed.Host = externalHost
		header.Set("Location", parsed.String())
		return nil
	}
	hostname := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if strings.HasSuffix(hostname, ".svc") || strings.HasSuffix(hostname, ".svc.cluster.local") {
		return fmt.Errorf("upstream redirect exposed an internal service")
	}
	return nil
}

func isWebSocket(request *http.Request) bool {
	return headerHasToken(request.Header.Values("Connection"), "upgrade") && strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
}

func validWebSocketOrigin(request *http.Request, requestHost string) bool {
	values := request.Header.Values("Origin")
	if len(values) != 1 {
		return false
	}
	origin, err := url.Parse(values[0])
	if err != nil || !strings.EqualFold(origin.Scheme, "https") || origin.User != nil || origin.Host == "" || origin.Path != "" || origin.RawPath != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
		return false
	}
	originHost, err := auth.CanonicalHost(origin.Host)
	return err == nil && originHost == requestHost
}

func headerHasToken(values []string, wanted string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), wanted) {
				return true
			}
		}
	}
	return false
}

func setSecurityHeaders(header http.Header) {
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	header.Set("X-Frame-Options", "DENY")
}

func writePlain(response http.ResponseWriter, status int, message string) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_, _ = io.WriteString(response, message+"\n")
}

// BootstrapURL is the only helper that places a raw bootstrap capability in a
// fragment. Callers must return it directly and must not log it.
func BootstrapURL(host, capability string) (string, error) {
	canonical, err := auth.CanonicalHost(host)
	if err != nil {
		return "", err
	}
	if len(capability) != 43 {
		return "", fmt.Errorf("invalid bootstrap capability")
	}
	return "https://" + canonical + "/#asp=" + url.QueryEscape(capability), nil
}

// BootstrapDocument exposes the exact fixed document for tests and Colima
// evidence without pretending that unit tests execute fragment JavaScript.
func BootstrapDocument() string { return bootstrapHTML }

// BootstrapEndpoint exposes the reserved exchange path for integration tests.
func BootstrapEndpoint() string { return bootstrapPath }
