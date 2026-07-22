package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	workspace := t.TempDir()
	manager, err := NewManager(Config{StateDir: t.TempDir(), WorkspaceDir: workspace, ChildUID: -1, ChildGID: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func startRequest(id string, argv ...string) Request {
	return Request{Version: ProtocolVersion, Operation: "start", RequestID: id, Argv: argv, Cwd: "/workspace", Env: map[string]string{"LANG": "C"}}
}

func TestNormalizeRejectsUnsafeSpecs(t *testing.T) {
	tests := []Request{
		{RequestID: "x", Argv: nil, Cwd: "/workspace"},
		{RequestID: "x", Argv: []string{"x"}, Cwd: "/tmp"},
		{RequestID: "x", Argv: []string{"x\x00"}, Cwd: "/workspace"},
		{RequestID: "x", Argv: []string{"x"}, Cwd: "/workspace", Env: map[string]string{"A=B": "x"}},
	}
	for i, request := range tests {
		if _, err := normalize(request); err == nil {
			t.Fatalf("case %d unexpectedly accepted", i)
		}
	}
}

func TestStartIdempotencyConflictAndReplay(t *testing.T) {
	manager := testManager(t)
	request := startRequest("same-request", "/bin/sh", "-c", "printf out; printf err >&2")
	first := manager.Start(request)
	if !first.OK {
		t.Fatalf("start: %+v", first)
	}
	second := manager.Start(request)
	if !second.OK || second.Command.ID != first.Command.ID {
		t.Fatalf("idempotent start: %+v", second)
	}
	conflict := manager.Start(startRequest("same-request", "/bin/echo", "different"))
	if conflict.Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("conflict: %+v", conflict)
	}
	wait := manager.Wait(context.Background(), first.Command.ID, 5*time.Second)
	if !wait.OK || wait.Command.State != StateExited {
		t.Fatalf("wait: %+v", wait)
	}
	connected := manager.Connect(first.Command.ID, 0)
	if !connected.OK || len(connected.Events) != 2 {
		t.Fatalf("connect: %+v", connected)
	}
	for i, event := range connected.Events {
		if event.Seq != uint64(i+1) {
			t.Fatalf("non-monotonic seq: %+v", connected.Events)
		}
	}
	streams := map[string]bool{}
	for _, event := range connected.Events {
		streams[event.Stream] = true
	}
	if !streams["stdout"] || !streams["stderr"] {
		t.Fatalf("missing stream: %+v", connected.Events)
	}
}

func TestConcurrentIdempotentStartCreatesOneCommand(t *testing.T) {
	manager := testManager(t)
	request := startRequest("concurrent", "/bin/sh", "-c", "exit 0")
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := manager.Start(request)
			if !response.OK {
				t.Errorf("start: %+v", response)
				return
			}
			ids <- response.Command.ID
		}()
	}
	wg.Wait()
	close(ids)
	var want string
	for id := range ids {
		if want == "" {
			want = id
		}
		if id != want {
			t.Fatalf("different IDs %q and %q", want, id)
		}
	}
	if got := len(manager.List().Commands); got != 1 {
		t.Fatalf("got %d commands", got)
	}
	_ = manager.Wait(context.Background(), want, 5*time.Second)
}

func TestStdinCloseAndTerminalAfterOutputEOF(t *testing.T) {
	manager := testManager(t)
	response := manager.Start(startRequest("stdin", "/bin/sh", "-c", "cat"))
	if !response.OK {
		t.Fatalf("start: %+v", response)
	}
	if result := manager.Stdin(response.Command.ID, []byte("hello")); !result.OK {
		t.Fatalf("stdin: %+v", result)
	}
	if result := manager.CloseStdin(response.Command.ID); !result.OK {
		t.Fatalf("close: %+v", result)
	}
	wait := manager.Wait(context.Background(), response.Command.ID, 5*time.Second)
	if !wait.OK {
		t.Fatalf("wait: %+v", wait)
	}
	connected := manager.Connect(response.Command.ID, 0)
	if len(connected.Events) != 1 || string(connected.Events[0].Data) != "hello" {
		t.Fatalf("events: %+v", connected.Events)
	}
}

func TestBoundedSpoolEvictsAndExpiresCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool")
	for seq := uint64(1); seq <= 5; seq++ {
		if err := appendBoundedEvent(path, Event{Seq: seq, Stream: "stdout", Data: bytes.Repeat([]byte{'x'}, 20)}, 70); err != nil {
			t.Fatal(err)
		}
	}
	events, err := readEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := segments(path)
	if err != nil {
		t.Fatal(err)
	}
	var spoolBytes int64
	for _, part := range parts {
		spoolBytes += part.size
	}
	if spoolBytes > 70 {
		t.Fatalf("spool is %d bytes", spoolBytes)
	}
	if events[0].Seq <= 1 {
		t.Fatalf("old cursor was not evicted: %+v", events)
	}
	manager := testManager(t)
	state := &processState{record: Command{ID: "c", State: StateExited, NextSeq: 6}, changed: make(chan struct{})}
	manager.commands["c"] = state
	commandSpool := filepath.Join(manager.commandDir("c"), "spool")
	if err := os.MkdirAll(commandSpool, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commandSpool, fmt.Sprintf("%020d.bin", events[0].Seq)), encodeEvents(events), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := manager.Connect("c", 0); got.Code != "CURSOR_EXPIRED" {
		t.Fatalf("connect: %+v", got)
	}
	if got := manager.Connect("c", 99); got.Code != "INVALID_CURSOR" {
		t.Fatalf("connect: %+v", got)
	}
}

func TestEightMiBCommandDiskBudgetIncludesSpoolHeaders(t *testing.T) {
	dir := t.TempDir()
	budget := MaxSpoolBytes - diskOverheadReserve
	payload := bytes.Repeat([]byte{'x'}, MaxEventBytes)
	for seq := uint64(1); seq <= 530; seq++ {
		if err := appendBoundedEvent(dir, Event{Seq: seq, Stream: "stdout", Data: payload}, budget); err != nil {
			t.Fatal(err)
		}
	}
	parts, err := segments(dir)
	if err != nil {
		t.Fatal(err)
	}
	var bytesOnDisk int64
	for _, part := range parts {
		bytesOnDisk += part.size
	}
	if bytesOnDisk > int64(budget) {
		t.Fatalf("spool files use %d bytes, budget %d", bytesOnDisk, budget)
	}
	if bytesOnDisk+diskOverheadReserve > MaxSpoolBytes {
		t.Fatalf("command state exceeds %d-byte cap", MaxSpoolBytes)
	}
	events, err := readEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Seq == 1 {
		t.Fatal("expected whole-segment eviction after exceeding cap")
	}
}

func TestSpoolRecoveryTruncatesPartialFinalRecord(t *testing.T) {
	dir := t.TempDir()
	complete := encodeEvent(Event{Seq: 1, Stream: "stdout", Data: []byte("complete")})
	partial := encodeEvent(Event{Seq: 2, Stream: "stderr", Data: []byte("partial")})
	path := filepath.Join(dir, "00000000000000000001.bin")
	if err := os.WriteFile(path, append(append(append([]byte(nil), spoolVersionHeader...), complete...), partial[:len(partial)-3]...), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := readEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || string(events[0].Data) != "complete" {
		t.Fatalf("recovered events: %+v", events)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(spoolVersionHeader)+len(complete)) {
		t.Fatalf("partial tail was not truncated: %d", info.Size())
	}
}

func TestRecoveryMarksInflightLostAndRetainsOutput(t *testing.T) {
	stateDir, workspace := t.TempDir(), t.TempDir()
	commandID := strings.Repeat("ab", 24)
	commandDir := filepath.Join(stateDir, "commands", commandID)
	if err := os.MkdirAll(commandDir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := Command{ID: commandID, RequestID: "req", Argv: []string{"/bin/sleep", "10"}, Cwd: "/workspace", SpecHash: "hash", State: StateRunning, CreatedAt: time.Now(), NextSeq: 1}
	if err := atomicJSON(filepath.Join(commandDir, "metadata.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	spoolDir := filepath.Join(commandDir, "spool")
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := atomicBytes(filepath.Join(spoolDir, "00000000000000000001.bin"), encodeEvents([]Event{{Seq: 1, Stream: "stdout", Data: []byte("retained")}}), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{StateDir: stateDir, WorkspaceDir: workspace, ChildUID: -1, ChildGID: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	status := manager.Status(commandID)
	if !status.OK || status.Command.State != StateLost || status.Command.NextSeq != 2 {
		t.Fatalf("status: %+v", status)
	}
	connected := manager.Connect(commandID, 0)
	if !connected.OK || string(connected.Events[0].Data) != "retained" {
		t.Fatalf("replay: %+v", connected)
	}
	data, err := os.ReadFile(filepath.Join(commandDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "pid") {
		t.Fatalf("persisted metadata exposes PID: %s", data)
	}
	var persisted Command
	if err = json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.State != StateLost {
		t.Fatalf("persisted state %q", persisted.State)
	}
}

func TestWorkspaceSymlinkEscapeRejected(t *testing.T) {
	manager := testManager(t)
	if err := os.Symlink("/", filepath.Join(manager.cfg.WorkspaceDir, "escape")); err != nil {
		t.Fatal(err)
	}
	request := startRequest("escape", "/bin/true")
	request.Cwd = "/workspace/escape/tmp"
	response := manager.Start(request)
	if response.Code != "INVALID_ARGUMENT" {
		t.Fatalf("escape accepted: %+v", response)
	}
}
