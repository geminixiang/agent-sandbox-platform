package httpapi_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

type contractClient struct{ baseURL, token string }

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
