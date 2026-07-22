package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/httpapi"
)

func TestReadinessTraversesBackendCheck(t *testing.T) {
	backend := newFakeBackend()
	ready := false
	server := httptest.NewServer(httpapi.New(backend, func(string) (string, bool) { return "", false }, func(context.Context) error {
		if !ready {
			return errors.New("Kubernetes unavailable")
		}
		return nil
	}))
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.StatusCode)
	}

	ready = true
	response, err = http.Get(server.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
}
