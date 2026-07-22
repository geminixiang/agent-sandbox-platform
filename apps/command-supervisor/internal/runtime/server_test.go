package runtime

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func socketTestManager(t *testing.T) *Manager {
	t.Helper()
	stateDir, err := os.MkdirTemp("/tmp", "asp-socket-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(stateDir) })
	manager, err := NewManager(Config{StateDir: stateDir, WorkspaceDir: t.TempDir(), ChildUID: -1, ChildGID: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func TestLengthPrefixedUnixRoundTrip(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "asp-socket-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(stateDir) })
	manager, err := NewManager(Config{StateDir: stateDir, WorkspaceDir: t.TempDir(), ChildUID: -1, ChildGID: -1})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(manager, filepath.Join(manager.cfg.StateDir, "supervisor.sock"))
	listener, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	request, _ := json.Marshal(Request{Version: ProtocolVersion, Operation: "health"})
	body, err := RoundTrip(server.socket, request)
	if err != nil {
		t.Fatal(err)
	}
	var response Response
	if err = json.Unmarshal(body, &response); err != nil || !response.OK {
		t.Fatalf("response: %s (%v)", body, err)
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDispatchTypedValidationErrors(t *testing.T) {
	server := NewServer(testManager(t), "unused")
	tests := []struct {
		request Request
		code    string
	}{
		{Request{Version: 999, Operation: "health"}, "UNSUPPORTED_VERSION"},
		{Request{Version: ProtocolVersion, Operation: "unknown"}, "UNKNOWN_OPERATION"},
		{Request{Version: ProtocolVersion, Operation: "status", CommandID: "missing"}, "NOT_FOUND"},
		{Request{Version: ProtocolVersion, Operation: "signal", CommandID: "missing", Signal: "INT"}, "NOT_FOUND"},
	}
	for _, test := range tests {
		if got := server.Dispatch(context.Background(), test.request); got.Code != test.code {
			t.Fatalf("got %+v, want %s", got, test.code)
		}
	}
}

func TestCommandRecordLimit(t *testing.T) {
	manager := testManager(t)
	for i := range MaxCommands {
		id := fmt.Sprintf("%048x", i)
		manager.commands[id] = &processState{record: Command{ID: id, State: StateExited}, changed: make(chan struct{})}
	}
	response := manager.Start(startRequest("over-cap", "/bin/sh", "-c", "true"))
	if response.Code != "COMMAND_LIMIT" {
		t.Fatalf("limit: %+v", response)
	}
}

func TestWaitTimeoutAndSignalValidation(t *testing.T) {
	manager := testManager(t)
	response := manager.Start(startRequest("wait", "/bin/sh", "-c", "sleep 5"))
	if !response.OK {
		t.Fatalf("start: %+v", response)
	}
	if got := manager.Wait(context.Background(), response.Command.ID, time.Millisecond); got.Code != "WAIT_TIMEOUT" {
		t.Fatalf("wait: %+v", got)
	}
	if got := manager.Signal(response.Command.ID, "INT"); got.Code != "INVALID_SIGNAL" {
		t.Fatalf("signal: %+v", got)
	}
	if got := manager.Signal(response.Command.ID, "KILL"); !got.OK {
		t.Fatalf("cleanup: %+v", got)
	}
	_ = manager.Wait(context.Background(), response.Command.ID, 5*time.Second)
}

func TestServerBoundsStalledMidFrameClients(t *testing.T) {
	manager := socketTestManager(t)
	server := NewServer(manager, filepath.Join(manager.cfg.StateDir, "supervisor.sock"))
	server.maxClients = 2
	server.readTimeout = 5 * time.Second
	listener, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	stalled := make([]net.Conn, 0, 2)
	for range 2 {
		conn, err := net.Dial("unix", server.socket)
		if err != nil {
			t.Fatal(err)
		}
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], 100)
		_, _ = conn.Write(header[:])
		_, _ = conn.Write([]byte{'{'})
		stalled = append(stalled, conn)
	}
	time.Sleep(50 * time.Millisecond)

	third, err := net.Dial("unix", server.socket)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(Request{Version: ProtocolVersion, Operation: "health"})
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(request)))
	_, _ = third.Write(header[:])
	_, _ = third.Write(request)
	_ = third.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, err = io.ReadFull(third, header[:]); err == nil {
		t.Fatal("server exceeded root-client handler bound")
	}
	_ = stalled[0].Close()
	_ = third.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = io.ReadFull(third, header[:]); err != nil {
		t.Fatalf("released client slot was leaked: %v", err)
	}
	body := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err = io.ReadFull(third, body); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err = json.Unmarshal(body, &response); err != nil || !response.OK {
		t.Fatalf("response: %s (%v)", body, err)
	}
	_ = third.Close()
	_ = stalled[1].Close()
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDisconnectedWaitReleasesClientSlot(t *testing.T) {
	manager := socketTestManager(t)
	started := manager.Start(startRequest("disconnected-wait", "/bin/sh", "-c", "sleep 30"))
	if !started.OK {
		t.Fatalf("start: %+v", started)
	}
	defer func() {
		_ = manager.Signal(started.Command.ID, "KILL")
		_ = manager.Wait(context.Background(), started.Command.ID, 5*time.Second)
	}()
	server := NewServer(manager, filepath.Join(manager.cfg.StateDir, "supervisor.sock"))
	server.maxClients = 1
	listener, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	conn, err := net.Dial("unix", server.socket)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(Request{Version: ProtocolVersion, Operation: "wait", CommandID: started.Command.ID, TimeoutMS: 300000})
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(request)))
	_, _ = conn.Write(header[:])
	_, _ = conn.Write(request)
	time.Sleep(50 * time.Millisecond)
	_ = conn.Close()

	health, _ := json.Marshal(Request{Version: ProtocolVersion, Operation: "health"})
	result := make(chan error, 1)
	go func() {
		_, err := RoundTrip(server.socket, health)
		result <- err
	}()
	select {
	case err = <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait remained detached from its disconnected client")
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWriteFrameRejectsOversizedResponse(t *testing.T) {
	err := writeFrame(io.Discard, Response{Version: ProtocolVersion, OK: true, Message: strings.Repeat("x", MaxResponseBytes)})
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestResponseNeverSerializesEnvironmentOrPID(t *testing.T) {
	manager := testManager(t)
	response := manager.Start(startRequest("redaction", "/bin/sh", "-c", "true"))
	if !response.OK {
		t.Fatalf("start: %+v", response)
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "LANG=C") || strings.Contains(strings.ToLower(string(body)), "pid") {
		t.Fatalf("private execution data exposed: %s", body)
	}
	_ = manager.Wait(context.Background(), response.Command.ID, 5*time.Second)
}
