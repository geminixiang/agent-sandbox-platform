package httpapi_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/auth"
	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/httpapi"
	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

const secret = "test-secret"

func TestLeaseLifecycleContract(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(func() { _ = backend.Close(context.Background()) })
	server := httptest.NewServer(httpapi.New(backend, auth.SecretResolver(func(id string) (string, bool) {
		return secret, id == "mikan"
	})))
	t.Cleanup(server.Close)
	client := contractClient{baseURL: server.URL, token: token("mikan", "subject-a", secret)}

	created := client.request(t, http.MethodPost, "/v1/leases", `{"pool":"local"}`, map[string]string{"Idempotency-Key": "request-1"}, 201)
	lease := created["lease"].(map[string]any)
	id := lease["id"].(string)
	if created["replayed"] != false || lease["status"] != "active" {
		t.Fatalf("unexpected create response: %#v", created)
	}

	client.request(t, http.MethodPost, "/v1/leases/"+id+"/files/write", `{"path":"/workspace/message.txt","content":"hello platform"}`, nil, 200)
	read := client.request(t, http.MethodPost, "/v1/leases/"+id+"/files/read", `{"path":"/workspace/message.txt","encoding":"utf8"}`, nil, 200)
	if read["content"] != "hello platform" {
		t.Fatalf("unexpected file content: %#v", read)
	}
	execResult := client.request(t, http.MethodPost, "/v1/leases/"+id+"/exec", `{"command":"cat message.txt"}`, nil, 200)
	if execResult["stdout"] != "hello platform" || execResult["code"] != float64(0) {
		t.Fatalf("unexpected exec response: %#v", execResult)
	}

	replay := client.request(t, http.MethodPost, "/v1/leases", `{"pool":"local"}`, map[string]string{"Idempotency-Key": "request-1"}, 201)
	if replay["replayed"] != true || replay["lease"].(map[string]any)["id"] != id {
		t.Fatalf("unexpected replay: %#v", replay)
	}
	client.request(t, http.MethodPost, "/v1/leases/"+id+"/release", `{}`, nil, 200)
	errorBody := client.request(t, http.MethodPost, "/v1/leases/"+id+"/exec", `{"command":"true"}`, nil, 409)
	if errorBody["error"].(map[string]any)["code"] != "LEASE_NOT_ACTIVE" {
		t.Fatalf("unexpected error: %#v", errorBody)
	}
	client.request(t, http.MethodDelete, "/v1/leases/"+id, "", nil, 204)
}

func TestStreamingFileContract(t *testing.T) {
	backend := newFakeBackend()
	server := httptest.NewServer(httpapi.New(backend, func(id string) (string, bool) { return secret, id == "mikan" }))
	t.Cleanup(server.Close)
	client := contractClient{server.URL, token("mikan", "owner", secret)}
	created := client.request(t, http.MethodPost, "/v1/leases", `{"pool":"local"}`, map[string]string{"Idempotency-Key": "stream-1"}, 201)
	id := created["lease"].(map[string]any)["id"].(string)
	content := []byte("first chunk|second chunk")
	digest := sha256.Sum256(content)

	response := client.rawRequest(t, http.MethodPut, "/v1/leases/"+id+"/files/content?path=%2Fworkspace%2Fdata.bin", bytes.NewReader(content), map[string]string{
		"Content-Type": "application/octet-stream", "Content-Digest": contentDigest(digest), "Content-Length": fmt.Sprint(len(content)),
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("PUT status %d: %s", response.StatusCode, payload)
	}

	response = client.rawRequest(t, http.MethodGet, "/v1/leases/"+id+"/files/content?path=%2Fworkspace%2Fdata.bin", nil, nil)
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !bytes.Equal(payload, content) {
		t.Fatalf("GET status=%d body=%q", response.StatusCode, payload)
	}
	if response.Header.Get("Content-Type") != "application/octet-stream" || response.Header.Get("Content-Length") != fmt.Sprint(len(content)) || response.Header.Get("Content-Digest") != contentDigest(digest) {
		t.Fatalf("unexpected streaming headers: %#v", response.Header)
	}

	bad := digest
	bad[0] ^= 0xff
	response = client.rawRequest(t, http.MethodPut, "/v1/leases/"+id+"/files/content?path=%2Fworkspace%2Fdata.bin", bytes.NewReader([]byte("replacement")), map[string]string{
		"Content-Type": "application/octet-stream", "Content-Digest": contentDigest(bad), "Content-Length": "11",
	})
	defer response.Body.Close()
	assertErrorResponse(t, response, 422, "CONTENT_DIGEST_MISMATCH")
	backend.mu.Lock()
	preserved := backend.leases[id].files["/workspace/data.bin"]
	backend.mu.Unlock()
	if preserved != string(content) {
		t.Fatalf("digest failure replaced destination: %q", preserved)
	}
}

func TestStreamingValidationAndUnsupportedBackend(t *testing.T) {
	backend := newFakeBackend()
	handler := httpapi.New(backend, func(id string) (string, bool) { return secret, id == "mikan" })
	client := contractClient{token: token("mikan", "owner", secret)}

	tests := []struct {
		name    string
		headers map[string]string
		length  int64
		status  int
		code    string
	}{
		{"missing length", map[string]string{"Content-Type": "application/octet-stream", "Content-Digest": contentDigest(sha256.Sum256(nil))}, -1, 411, "LENGTH_REQUIRED"},
		{"media type", map[string]string{"Content-Type": "text/plain", "Content-Digest": contentDigest(sha256.Sum256(nil))}, 0, 415, "UNSUPPORTED_MEDIA_TYPE"},
		{"digest", map[string]string{"Content-Type": "application/octet-stream", "Content-Digest": "sha-256=:bad:"}, 0, 400, "INVALID_CONTENT_DIGEST"},
		{"oversize", map[string]string{"Content-Type": "application/octet-stream", "Content-Digest": contentDigest(sha256.Sum256(nil))}, httpapi.MaxFileTransferBytes + 1, 413, "TRANSFER_TOO_LARGE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/v1/leases/id/files/content?path=/workspace/a", http.NoBody)
			request.Header.Set("Authorization", "Bearer "+client.token)
			for key, value := range test.headers {
				request.Header.Set(key, value)
			}
			request.ContentLength = test.length
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			assertRecordedError(t, recorder.Result(), test.status, test.code)
		})
	}

	active, err := backend.Acquire(context.Background(), lease.Scope{ConsumerID: "mikan", SubjectID: "owner"}, lease.AcquireRequest{Pool: "local", IdempotencyKey: "mismatch"})
	if err != nil {
		t.Fatal(err)
	}
	mismatch := httptest.NewRequest(http.MethodPut, "/v1/leases/"+active.Lease.ID+"/files/content?path=/workspace/a", strings.NewReader("ab"))
	mismatch.Header.Set("Authorization", "Bearer "+client.token)
	mismatch.Header.Set("Content-Type", "application/octet-stream")
	mismatch.Header.Set("Content-Digest", contentDigest(sha256.Sum256([]byte("ab"))))
	mismatch.ContentLength = 3
	mismatchRecorder := httptest.NewRecorder()
	handler.ServeHTTP(mismatchRecorder, mismatch)
	assertRecordedError(t, mismatchRecorder.Result(), 400, "CONTENT_LENGTH_MISMATCH")

	legacy := legacyOnlyBackend{Backend: backend}
	unsupported := httpapi.New(legacy, func(id string) (string, bool) { return secret, id == "mikan" })
	request := httptest.NewRequest(http.MethodGet, "/v1/leases/id/files/content?path=/workspace/a", nil)
	request.Header.Set("Authorization", "Bearer "+client.token)
	recorder := httptest.NewRecorder()
	unsupported.ServeHTTP(recorder, request)
	assertRecordedError(t, recorder.Result(), 501, "STREAMING_NOT_SUPPORTED")
}

func TestStreamingPreflightCrossScopeAndPostCommitFailure(t *testing.T) {
	backend := newFakeBackend()
	handler := httpapi.New(backend, func(id string) (string, bool) { return secret, id == "mikan" })
	owner := contractClient{token: token("mikan", "owner", secret)}
	attacker := contractClient{token: token("mikan", "attacker", secret)}
	create := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/leases", strings.NewReader(`{"pool":"local"}`))
	request.Header.Set("Authorization", "Bearer "+owner.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "stream-owner")
	handler.ServeHTTP(create, request)
	var payload map[string]any
	_ = json.Unmarshal(create.Body.Bytes(), &payload)
	id := payload["lease"].(map[string]any)["id"].(string)

	for _, path := range []string{"/v1/leases/" + id + "/files/content?path=/workspace/a", "/v1/leases/missing/files/content?path=/workspace/a"} {
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			recorder := httptest.NewRecorder()
			request = httptest.NewRequest(method, path, http.NoBody)
			request.Header.Set("Authorization", "Bearer "+attacker.token)
			if method == http.MethodPut {
				request.Header.Set("Content-Type", "application/octet-stream")
				request.Header.Set("Content-Digest", contentDigest(sha256.Sum256(nil)))
				request.ContentLength = 0
			}
			handler.ServeHTTP(recorder, request)
			assertRecordedError(t, recorder.Result(), 404, "LEASE_NOT_FOUND")
		}
	}

	recorder := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/leases/"+id+"/files/content?path=/workspace/preflight-error", nil)
	request.Header.Set("Authorization", "Bearer "+owner.token)
	handler.ServeHTTP(recorder, request)
	assertRecordedError(t, recorder.Result(), 404, "FILE_NOT_FOUND")

	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/leases/"+id+"/files/content?path=/workspace/reader-error", nil)
	request.Header.Set("Authorization", "Bearer "+owner.token)
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		handler.ServeHTTP(recorder, request)
	}()
	if recovered != http.ErrAbortHandler {
		t.Fatalf("panic = %#v, want http.ErrAbortHandler", recovered)
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte(`{"error"`)) {
		t.Fatalf("post-commit JSON appended to binary body: %q", recorder.Body.Bytes())
	}
}

func TestListHTTPValidatesQueriesAndPreservesEmptyPages(t *testing.T) {
	base := newFakeBackend()
	next := "opaque-next"
	backend := &listBackend{fakeBackend: base, pages: []lease.Page{
		{Leases: []lease.Record{}, NextCursor: &next},
		{Leases: []lease.Record{}, NextCursor: nil},
	}}
	server := httptest.NewServer(httpapi.New(backend, func(id string) (string, bool) { return secret, id == "mikan" }))
	t.Cleanup(server.Close)
	client := contractClient{server.URL, token("mikan", "subject-a", secret)}

	first := client.request(t, http.MethodGet, "/v1/leases?pool=local&limit=1", "", nil, 200)
	if leases, ok := first["leases"].([]any); !ok || len(leases) != 0 || first["nextCursor"] != next {
		t.Fatalf("first page = %#v", first)
	}
	second := client.request(t, http.MethodGet, "/v1/leases?pool=local&limit=1&cursor="+next, "", nil, 200)
	if leases, ok := second["leases"].([]any); !ok || len(leases) != 0 || second["nextCursor"] != nil {
		t.Fatalf("second page = %#v", second)
	}
	if len(backend.requests) != 2 || backend.requests[0] != (lease.ListRequest{Pool: "local", Limit: 1}) || backend.requests[1].Cursor != next {
		t.Fatalf("list requests = %#v", backend.requests)
	}

	for _, path := range []string{
		"/v1/leases?limit=0",
		"/v1/leases?limit=101",
		"/v1/leases?limit=01",
		"/v1/leases?limit=1&limit=2",
		"/v1/leases?pool=",
		"/v1/leases?operator=true",
	} {
		body := client.request(t, http.MethodGet, path, "", nil, 400)
		if body["error"].(map[string]any)["code"] != "INVALID_REQUEST" {
			t.Fatalf("%s: body = %#v", path, body)
		}
	}
	for _, path := range []string{"/v1/leases?cursor=", "/v1/leases?cursor=" + strings.Repeat("x", lease.MaxListCursorBytes+1)} {
		body := client.request(t, http.MethodGet, path, "", nil, 400)
		if body["error"].(map[string]any)["code"] != "INVALID_CURSOR" {
			t.Fatalf("%s: body = %#v", path, body)
		}
	}
	body := client.request(t, http.MethodPost, "/v1/leases?limit=1", `{"pool":"local"}`, map[string]string{"Idempotency-Key": "request"}, 400)
	if body["error"].(map[string]any)["code"] != "INVALID_REQUEST" {
		t.Fatalf("POST query body = %#v", body)
	}
}

func TestListHTTPErrorsRemainTyped(t *testing.T) {
	backend := &listBackend{fakeBackend: newFakeBackend(), listError: lease.NewError(410, "CURSOR_EXPIRED", "List cursor has expired")}
	server := httptest.NewServer(httpapi.New(backend, func(id string) (string, bool) { return secret, id == "mikan" }))
	t.Cleanup(server.Close)
	client := contractClient{server.URL, token("mikan", "subject-a", secret)}
	body := client.request(t, http.MethodGet, "/v1/leases?cursor=opaque", "", nil, 410)
	if body["error"].(map[string]any)["code"] != "CURSOR_EXPIRED" {
		t.Fatalf("body = %#v", body)
	}
}

func TestCrossSubjectAccessIsIndistinguishableFromMissingLease(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(func() { _ = backend.Close(context.Background()) })
	server := httptest.NewServer(httpapi.New(backend, func(id string) (string, bool) { return secret, id == "mikan" }))
	t.Cleanup(server.Close)
	owner := contractClient{server.URL, token("mikan", "owner", secret)}
	attacker := contractClient{server.URL, token("mikan", "attacker", secret)}
	created := owner.request(t, http.MethodPost, "/v1/leases", `{"pool":"local"}`, map[string]string{"Idempotency-Key": "request-1"}, 201)
	id := created["lease"].(map[string]any)["id"].(string)
	for _, path := range []string{"/v1/leases/" + id, "/v1/leases/lease_missing"} {
		body := attacker.request(t, http.MethodGet, path, "", nil, 404)
		errorBody := body["error"].(map[string]any)
		if errorBody["code"] != "LEASE_NOT_FOUND" || errorBody["message"] != "Lease not found" {
			t.Fatalf("resource disclosure: %#v", body)
		}
	}
}

type listBackend struct {
	*fakeBackend
	pages     []lease.Page
	requests  []lease.ListRequest
	listError error
}

func (b *listBackend) List(_ context.Context, _ lease.Scope, request lease.ListRequest) (lease.Page, error) {
	b.requests = append(b.requests, request)
	if b.listError != nil {
		return lease.Page{}, b.listError
	}
	page := b.pages[0]
	b.pages = b.pages[1:]
	return page, nil
}

type contractClient struct{ baseURL, token string }

func (c contractClient) rawRequest(t *testing.T, method, path string, body io.Reader, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func (c contractClient) request(t *testing.T, method, path, body string, headers map[string]string, wantStatus int) map[string]any {
	t.Helper()
	request, err := http.NewRequest(method, c.baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, response.StatusCode, wantStatus, payload)
	}
	if len(payload) == 0 {
		return nil
	}
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatalf("invalid JSON %q: %v", payload, err)
	}
	return value
}

func assertErrorResponse(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("invalid error JSON %q: %v", payload, err)
	}
	if response.StatusCode != status || body["error"].(map[string]any)["code"] != code {
		t.Fatalf("status=%d body=%s, want %d %s", response.StatusCode, payload, status, code)
	}
}

func assertRecordedError(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	defer response.Body.Close()
	assertErrorResponse(t, response, status, code)
}

func contentDigest(digest [sha256.Size]byte) string {
	return "sha-256=:" + base64.StdEncoding.EncodeToString(digest[:]) + ":"
}

func token(consumerID, subjectID, signingSecret string) string {
	claims, _ := json.Marshal(map[string]any{"consumerId": consumerID, "subjectId": subjectID, "exp": time.Now().Add(time.Minute).Unix()})
	payload := base64.RawURLEncoding.EncodeToString(claims)
	signed := fmt.Sprintf("v1.%s", payload)
	mac := hmac.New(sha256.New, []byte(signingSecret))
	_, _ = mac.Write([]byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type fakeLease struct {
	record lease.Record
	scope  lease.Scope
	files  map[string]string
	key    string
}

type fakeBackend struct {
	mu     sync.Mutex
	leases map[string]*fakeLease
	keys   map[string]string
	nextID int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{leases: make(map[string]*fakeLease), keys: make(map[string]string)}
}

func (b *fakeBackend) Acquire(_ context.Context, scope lease.Scope, request lease.AcquireRequest) (lease.AcquireResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := scope.ConsumerID + "\x00" + scope.SubjectID + "\x00" + request.IdempotencyKey
	if id := b.keys[key]; id != "" {
		return lease.AcquireResult{Lease: b.leases[id].record, Replayed: true}, nil
	}
	b.nextID++
	now := time.Now().UTC()
	record := lease.Record{ID: fmt.Sprintf("lease_%d", b.nextID), Pool: request.Pool, Status: lease.StatusActive, CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute), LastUsedAt: now}
	b.leases[record.ID] = &fakeLease{record: record, scope: scope, files: make(map[string]string), key: key}
	b.keys[key] = record.ID
	return lease.AcquireResult{Lease: record}, nil
}

func (b *fakeBackend) List(_ context.Context, scope lease.Scope, request lease.ListRequest) (lease.Page, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	records := make([]lease.Record, 0)
	for _, value := range b.leases {
		if value.scope == scope && value.record.Status == lease.StatusActive && (request.Pool == "" || value.record.Pool == request.Pool) {
			records = append(records, value.record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return lease.Page{Leases: records}, nil
}

func (b *fakeBackend) Get(_ context.Context, scope lease.Scope, id string) (lease.Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, err := b.require(scope, id)
	if err != nil {
		return lease.Record{}, err
	}
	return value.record, nil
}

func (b *fakeBackend) Exec(_ context.Context, scope lease.Scope, id string, request lease.ExecRequest) (lease.ExecResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, err := b.requireActive(scope, id)
	if err != nil {
		return lease.ExecResult{}, err
	}
	if request.Command == "cat message.txt" {
		return lease.ExecResult{Stdout: value.files["/workspace/message.txt"]}, nil
	}
	return lease.ExecResult{}, nil
}

func (b *fakeBackend) ReadFile(_ context.Context, scope lease.Scope, id string, request lease.ReadFileRequest) (lease.ReadFileResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, err := b.requireActive(scope, id)
	if err != nil {
		return lease.ReadFileResult{}, err
	}
	return lease.ReadFileResult{Path: request.Path, Content: value.files[request.Path], Encoding: "utf8"}, nil
}

func (b *fakeBackend) WriteFile(_ context.Context, scope lease.Scope, id string, request lease.WriteFileRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, err := b.requireActive(scope, id)
	if err != nil {
		return err
	}
	value.files[request.Path] = request.Content
	return nil
}

func (b *fakeBackend) OpenFile(_ context.Context, scope lease.Scope, id, path string) (lease.FileDownload, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, err := b.requireActive(scope, id)
	if err != nil {
		return lease.FileDownload{}, err
	}
	if path == "/workspace/preflight-error" {
		return lease.FileDownload{}, lease.NewError(404, "FILE_NOT_FOUND", "Workspace file not found")
	}
	if path == "/workspace/reader-error" {
		content := []byte("partial binary")
		return lease.FileDownload{Content: &failingReadCloser{content: content}, SizeBytes: int64(len(content) + 10), SHA256: sha256.Sum256(content)}, nil
	}
	content, ok := value.files[path]
	if !ok {
		return lease.FileDownload{}, lease.NewError(404, "FILE_NOT_FOUND", "Workspace file not found")
	}
	contentBytes := []byte(content)
	return lease.FileDownload{Content: io.NopCloser(bytes.NewReader(contentBytes)), SizeBytes: int64(len(contentBytes)), SHA256: sha256.Sum256(contentBytes)}, nil
}

func (b *fakeBackend) WriteFileStream(_ context.Context, scope lease.Scope, id string, request lease.StreamWriteRequest, content io.Reader) error {
	b.mu.Lock()
	_, err := b.requireActive(scope, id)
	b.mu.Unlock()
	if err != nil {
		return err
	}
	var temporary bytes.Buffer
	digest := sha256.New()
	count, err := io.Copy(io.MultiWriter(&temporary, digest), content)
	if err != nil {
		return err
	}
	if count != request.SizeBytes {
		return lease.NewError(400, "CONTENT_LENGTH_MISMATCH", "Request body does not match Content-Length")
	}
	if !hmac.Equal(digest.Sum(nil), request.SHA256[:]) {
		return lease.NewError(422, "CONTENT_DIGEST_MISMATCH", "Request body does not match Content-Digest")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	value, err := b.requireActive(scope, id)
	if err != nil {
		return err
	}
	value.files[request.Path] = temporary.String()
	return nil
}

type failingReadCloser struct {
	content []byte
	done    bool
}

func (r *failingReadCloser) Read(buffer []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(buffer, r.content), nil
	}
	return 0, fmt.Errorf("injected reader failure")
}
func (*failingReadCloser) Close() error { return nil }

type legacyOnlyBackend struct{ lease.Backend }

func (b *fakeBackend) Release(_ context.Context, scope lease.Scope, id string) (lease.Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, err := b.requireActive(scope, id)
	if err != nil {
		return lease.Record{}, err
	}
	value.record.Status = lease.StatusReleased
	delete(b.keys, value.key)
	return value.record, nil
}

func (b *fakeBackend) Delete(_ context.Context, scope lease.Scope, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, err := b.require(scope, id)
	if err != nil {
		return err
	}
	delete(b.keys, value.key)
	delete(b.leases, id)
	return nil
}

func (b *fakeBackend) Close(context.Context) error { return nil }

func (b *fakeBackend) require(scope lease.Scope, id string) (*fakeLease, error) {
	value := b.leases[id]
	if value == nil || value.scope != scope {
		return nil, lease.NotFound()
	}
	return value, nil
}

func (b *fakeBackend) requireActive(scope lease.Scope, id string) (*fakeLease, error) {
	value, err := b.require(scope, id)
	if err != nil {
		return nil, err
	}
	if value.record.Status != lease.StatusActive {
		return nil, lease.NewError(409, "LEASE_NOT_ACTIVE", "Lease is not active")
	}
	return value, nil
}
