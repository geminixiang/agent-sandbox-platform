package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testRecord = map[string]any{"id": "lease_1", "pool": "coding", "status": "active", "createdAt": "2026-01-01T00:00:00Z", "expiresAt": "2026-01-01T00:15:00Z", "lastUsedAt": "2026-01-01T00:00:00Z"}

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewClient(ClientOptions{BaseURL: server.URL, Credentials: StaticToken("subject-token")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
func jsonOut(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func TestLifecycleRunFilesAndCheckedError(t *testing.T) {
	var releases atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer subject-token" {
			t.Fatal("missing auth")
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/leases":
			jsonOut(w, 201, map[string]any{"lease": testRecord, "replayed": false})
		case strings.HasSuffix(r.URL.Path, "/files/write"):
			jsonOut(w, 200, map[string]string{"path": "/workspace/a"})
		case strings.HasSuffix(r.URL.Path, "/files/read"):
			jsonOut(w, 200, map[string]string{"content": "aGVsbG8=", "encoding": "base64"})
		case strings.HasSuffix(r.URL.Path, "/exec"):
			jsonOut(w, 200, map[string]any{"stdout": "out", "stderr": "err", "code": 17})
		case strings.HasSuffix(r.URL.Path, "/release"):
			releases.Add(1)
			released := cloneMap(testRecord)
			released["status"] = "released"
			jsonOut(w, 200, map[string]any{"lease": released})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	box, err := client.Create(context.Background(), CreateOptions{Pool: "coding"})
	if err != nil {
		t.Fatal(err)
	}
	if err = box.Files().WriteBytes(context.Background(), "a", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	value, err := box.Files().ReadBytes(context.Background(), "a")
	if err != nil || string(value) != "hello" {
		t.Fatalf("bytes=%q err=%v", value, err)
	}
	result, err := box.Run(context.Background(), "bad", RunOptions{Check: true})
	var commandErr *CommandFailedError
	if result.ExitCode != 17 || !errors.As(err, &commandErr) || !errors.Is(err, ErrCommandFailed) || commandErr.Result.Stderr != "err" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	done := make(chan error, 2)
	go func() { _, e := box.Release(context.Background()); done <- e }()
	go func() { done <- box.Close(context.Background()) }()
	if <-done != nil || <-done != nil || releases.Load() != 1 {
		t.Fatalf("release calls=%d", releases.Load())
	}
}

func TestRotatingCredentialsPagerAndErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer token-") {
			t.Fatal("auth")
		}
		if r.URL.Query().Get("cursor") == "" {
			jsonOut(w, 200, map[string]any{"leases": []any{}, "nextCursor": "next"})
			return
		}
		jsonOut(w, 200, map[string]any{"leases": []any{testRecord}, "nextCursor": nil})
	}))
	defer server.Close()
	client, err := NewClient(ClientOptions{BaseURL: server.URL, Credentials: TokenProviderFunc(func(ctx context.Context) (string, error) {
		calls.Add(1)
		return "token-" + time.Now().Format("150405.000"), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	pager := client.Pager(ListOptions{Pool: "coding", Limit: 1})
	if !pager.Next(context.Background()) || pager.Sandbox().ID() != "lease_1" || pager.Next(context.Background()) || pager.Err() != nil {
		t.Fatalf("pager err=%v", pager.Err())
	}
	if calls.Load() != 2 {
		t.Fatalf("credential calls=%d", calls.Load())
	}
	_ = client.Close()
	if _, err = client.ListPage(context.Background(), ListOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed=%v", err)
	}
}

func TestStreamingWireIntegrityAndEarlyClose(t *testing.T) {
	content := []byte("stream-content")
	digest := sha256.Sum256(content)
	digestHex := hex.EncodeToString(digest[:])
	digestHeader := "sha-256=:" + base64.StdEncoding.EncodeToString(digest[:]) + ":"
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", "14")
			w.Header().Set("Content-Digest", digestHeader)
			_, _ = w.Write(content)
			return
		}
		if r.Header.Get("Content-Digest") != digestHeader || r.ContentLength != int64(len(content)) {
			t.Fatal("upload headers")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, content) {
			t.Fatal("body")
		}
		w.WriteHeader(204)
	})
	box := newSandbox(client, recordFromMap(t, testRecord))
	if err := box.Files().WriteStream(context.Background(), "a", bytes.NewReader(content), UploadOptions{SizeBytes: int64(len(content)), SHA256: digestHex}); err != nil {
		t.Fatal(err)
	}
	download, err := box.Files().ReadStream(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(download.Content)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("got=%q err=%v", got, err)
	}
	download, err = box.Files().ReadStream(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	one := make([]byte, 1)
	_, _ = download.Content.Read(one)
	if err = download.Content.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTypedProtocolErrors(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 404, map[string]any{"error": map[string]string{"code": "LEASE_NOT_FOUND", "message": "Lease not found"}})
	})
	_, err := client.Get(context.Background(), "missing")
	var sdkErr *Error
	if !errors.As(err, &sdkErr) || !errors.Is(err, ErrNotFound) || sdkErr.StatusCode != 404 {
		t.Fatalf("err=%v", err)
	}
}

func cloneMap(value map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range value {
		out[k] = v
	}
	return out
}
func recordFromMap(t *testing.T, value map[string]any) LeaseRecord {
	data, _ := json.Marshal(value)
	var record LeaseRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}
