package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

type runnerFunc func(context.Context, string, podTarget, []string, io.Reader, io.Writer, io.Writer) error

func (f runnerFunc) Run(ctx context.Context, namespace string, target podTarget, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return f(ctx, namespace, target, command, stdin, stdout, stderr)
}

func acquiredTransferBackend(t *testing.T, runner commandRunner, limits ...int) (*Backend, lease.AcquireResult) {
	t.Helper()
	backend, _ := fixture(t, "gvisor")
	backend.runner = runner
	global, perLease := 8, 2
	if len(limits) == 2 {
		global, perLease = limits[0], limits[1]
	}
	backend.transfers = newTransferManager(global, perLease, time.Minute)
	result, err := backend.Acquire(context.Background(), testScope(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	return backend, result
}

func TestOpenFilePreflightsThenStreamsExactBody(t *testing.T) {
	content := []byte("streamed without control-plane buffering")
	digest := sha256.Sum256(content)
	var command []string
	runner := runnerFunc(func(_ context.Context, _ string, _ podTarget, value []string, _ io.Reader, stdout, _ io.Writer) error {
		command = append([]string(nil), value...)
		_, _ = io.WriteString(stdout, "ASP1 OK "+strconv.Itoa(len(content))+" "+digestHex(digest)+"\n")
		_, _ = stdout.Write(content)
		return nil
	})
	backend, acquired := acquiredTransferBackend(t, runner)
	download, err := backend.OpenFile(context.Background(), testScope(), acquired.Lease.ID, "artifact.bin")
	if err != nil {
		t.Fatal(err)
	}
	if download.SizeBytes != int64(len(content)) || download.SHA256 != digest {
		t.Fatalf("metadata = %d %x", download.SizeBytes, download.SHA256)
	}
	body, err := io.ReadAll(download.Content)
	if err != nil || !bytes.Equal(body, content) {
		t.Fatalf("body = %q, error = %v", body, err)
	}
	if err := download.Content.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(command, " ") != transferHelper+" download /workspace/artifact.bin" {
		t.Fatalf("command = %#v", command)
	}
}

func TestOpenFileMapsPreflightAndPostPreflightFailures(t *testing.T) {
	for marker, want := range map[string]struct {
		status int
		code   string
	}{
		"ASP1 ERR FILE_NOT_FOUND\n":                   {404, "FILE_NOT_FOUND"},
		"ASP1 ERR INVALID_PATH\n":                     {400, "INVALID_PATH"},
		"ASP1 ERR TRANSFER_TOO_LARGE\n":               {413, "TRANSFER_TOO_LARGE"},
		strings.Repeat("x", maxTransferMarkerBytes+1): {502, "FILE_TRANSFER_FAILED"},
	} {
		t.Run(want.code, func(t *testing.T) {
			runner := runnerFunc(func(_ context.Context, _ string, _ podTarget, _ []string, _ io.Reader, stdout, _ io.Writer) error {
				_, _ = io.WriteString(stdout, marker)
				return errors.New("remote exit")
			})
			backend, acquired := acquiredTransferBackend(t, runner)
			_, err := backend.OpenFile(context.Background(), testScope(), acquired.Lease.ID, "file")
			status, code, _ := lease.ErrorDetails(err)
			if status != want.status || code != want.code {
				t.Fatalf("error = %d %s: %v", status, code, err)
			}
		})
	}

	content := []byte("partial")
	digest := sha256.Sum256(content)
	runner := runnerFunc(func(_ context.Context, _ string, _ podTarget, _ []string, _ io.Reader, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, "ASP1 OK 99 "+digestHex(digest)+"\n")
		_, _ = stdout.Write(content)
		return errors.New("remote body failure")
	})
	backend, acquired := acquiredTransferBackend(t, runner)
	download, err := backend.OpenFile(context.Background(), testScope(), acquired.Lease.ID, "file")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(download.Content)
	_, code, _ := lease.ErrorDetails(readErr)
	if string(body) != "partial" || code != "FILE_TRANSFER_FAILED" {
		t.Fatalf("body = %q, error = %v", body, readErr)
	}
	_ = download.Content.Close()
}

func TestWriteFileStreamPassesExactBodyAndMapsValidationMarkers(t *testing.T) {
	content := []byte("atomic upload")
	digest := sha256.Sum256(content)
	var received []byte
	var command []string
	runner := runnerFunc(func(_ context.Context, _ string, _ podTarget, value []string, stdin io.Reader, stdout, _ io.Writer) error {
		command = append([]string(nil), value...)
		received, _ = io.ReadAll(stdin)
		_, _ = io.WriteString(stdout, "ASP1 OK\n")
		return nil
	})
	backend, acquired := acquiredTransferBackend(t, runner)
	err := backend.WriteFileStream(context.Background(), testScope(), acquired.Lease.ID, lease.StreamWriteRequest{Path: "/workspace/a/file", SizeBytes: int64(len(content)), SHA256: digest}, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, content) || len(command) != 5 || command[0] != transferHelper || command[1] != "upload" || command[2] != "/workspace/a/file" || command[3] != strconv.Itoa(len(content)) || command[4] != digestHex(digest) {
		t.Fatalf("command = %#v body = %q", command, received)
	}

	for marker, code := range map[string]string{
		"ASP1 ERR CONTENT_LENGTH_MISMATCH\n": "CONTENT_LENGTH_MISMATCH",
		"ASP1 ERR CONTENT_DIGEST_MISMATCH\n": "CONTENT_DIGEST_MISMATCH",
		"ASP1 ERR INVALID_PATH\n":            "INVALID_PATH",
	} {
		backend.runner = runnerFunc(func(_ context.Context, _ string, _ podTarget, _ []string, stdin io.Reader, stdout, _ io.Writer) error {
			_, _ = io.Copy(io.Discard, stdin)
			_, _ = io.WriteString(stdout, marker)
			return errors.New("stable remote exit")
		})
		err := backend.WriteFileStream(context.Background(), testScope(), acquired.Lease.ID, lease.StreamWriteRequest{Path: "/workspace/file", SizeBytes: int64(len(content)), SHA256: digest}, bytes.NewReader(content))
		_, actual, _ := lease.ErrorDetails(err)
		if actual != code {
			t.Fatalf("marker %q mapped to %s: %v", marker, actual, err)
		}
	}
}

func TestTransferConcurrencyEarlyCloseAndLifecycleCancellationReleasePermits(t *testing.T) {
	started := make(chan struct{}, 8)
	runner := runnerFunc(func(ctx context.Context, _ string, _ podTarget, _ []string, _ io.Reader, stdout, _ io.Writer) error {
		digest := sha256.Sum256([]byte("x"))
		_, _ = io.WriteString(stdout, "ASP1 OK 1 "+digestHex(digest)+"\n")
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	backend, acquired := acquiredTransferBackend(t, runner, 2, 1)
	first, err := backend.OpenFile(context.Background(), testScope(), acquired.Lease.ID, "one")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	_, err = backend.OpenFile(context.Background(), testScope(), acquired.Lease.ID, "two")
	status, code, _ := lease.ErrorDetails(err)
	if status != 429 || code != "TRANSFER_LIMIT_REACHED" {
		t.Fatalf("limit error = %v", err)
	}
	if err := first.Content.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := backend.OpenFile(context.Background(), testScope(), acquired.Lease.ID, "two")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	released := make(chan error, 1)
	go func() {
		_, releaseErr := backend.Release(context.Background(), testScope(), acquired.Lease.ID)
		released <- releaseErr
	}()
	select {
	case err := <-released:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("release did not cancel and wait for active transfer")
	}
	_ = second.Content.Close()
	if backend.transfers.global != 0 {
		t.Fatalf("active permits = %d", backend.transfers.global)
	}
}

func TestTransferManagerGlobalLimitAndCloseRace(t *testing.T) {
	manager := newTransferManager(2, 2, time.Minute)
	expires := time.Now().Add(time.Hour)
	one, err := manager.begin(context.Background(), "one", expires)
	if err != nil {
		t.Fatal(err)
	}
	two, err := manager.begin(context.Background(), "two", expires)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.begin(context.Background(), "three", expires); leaseCode(err) != "TRANSFER_LIMIT_REACHED" {
		t.Fatalf("global limit error = %v", err)
	}
	go func() { <-one.ctx.Done(); one.finish() }()
	go func() { <-two.ctx.Done(); two.finish() }()
	if err := manager.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.global != 0 {
		t.Fatalf("close leaked %d permits", manager.global)
	}
	if _, err := manager.begin(context.Background(), "one", expires); leaseCode(err) != "ABORTED" {
		t.Fatalf("begin after close = %v", err)
	}
}

func TestTransferTimeoutCancelsRemoteCommand(t *testing.T) {
	runner := runnerFunc(func(ctx context.Context, _ string, _ podTarget, _ []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
		<-ctx.Done()
		return ctx.Err()
	})
	backend, acquired := acquiredTransferBackend(t, runner)
	backend.transfers = newTransferManager(1, 1, 20*time.Millisecond)
	_, err := backend.OpenFile(context.Background(), testScope(), acquired.Lease.ID, "slow")
	status, code, _ := lease.ErrorDetails(err)
	if status != 408 || code != "ABORTED" {
		t.Fatalf("timeout error = %v", err)
	}
	if backend.transfers.global != 0 {
		t.Fatalf("permit leaked after timeout: %d", backend.transfers.global)
	}
}

func TestTransferDeadlineNeverExceedsLeaseExpiry(t *testing.T) {
	manager := newTransferManager(1, 1, time.Hour)
	expires := time.Now().Add(50 * time.Millisecond)
	op, err := manager.begin(context.Background(), "lease", expires)
	if err != nil {
		t.Fatal(err)
	}
	defer op.finish()
	deadline, ok := op.ctx.Deadline()
	if !ok || deadline.After(expires) {
		t.Fatalf("deadline = %v, expiry = %v", deadline, expires)
	}
}

func digestHex(value [sha256.Size]byte) string { return hex.EncodeToString(value[:]) }
func leaseCode(err error) string               { _, code, _ := lease.ErrorDetails(err); return code }
