package runtime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

type faultPersistence struct {
	delegate      Persistence
	mu            sync.Mutex
	persistCalls  int
	failPersistAt int
	failAppend    bool
}

func (p *faultPersistence) PersistCommand(stateDir string, record *Command) error {
	p.mu.Lock()
	p.persistCalls++
	fail := p.persistCalls == p.failPersistAt
	p.mu.Unlock()
	if fail {
		return errors.New("injected metadata failure")
	}
	return p.delegate.PersistCommand(stateDir, record)
}

func (p *faultPersistence) AppendEvent(stateDir, commandID string, event Event, budget int) error {
	p.mu.Lock()
	fail := p.failAppend
	p.mu.Unlock()
	if fail {
		return errors.New("injected spool failure")
	}
	return p.delegate.AppendEvent(stateDir, commandID, event, budget)
}

func TestSecondManagerCannotRecoverOrMutateLiveState(t *testing.T) {
	stateDir, workspace := t.TempDir(), t.TempDir()
	first, err := NewManager(Config{StateDir: stateDir, WorkspaceDir: workspace, ChildUID: -1, ChildGID: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	started := first.Start(startRequest("live-owner", "/bin/sh", "-c", "sleep 30"))
	if !started.OK {
		t.Fatalf("start: %+v", started)
	}
	defer func() {
		_ = first.Signal(started.Command.ID, "KILL")
		_ = first.Wait(context.Background(), started.Command.ID, 5*time.Second)
	}()

	metadataPath := filepath.Join(first.commandDir(started.Command.ID), "metadata.json")
	before, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewManager(Config{StateDir: stateDir, WorkspaceDir: workspace, ChildUID: -1, ChildGID: -1}); err == nil {
		t.Fatal("second manager acquired live state")
	}
	after, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed startup mutated live metadata")
	}
	status := first.Status(started.Command.ID)
	if !status.OK || status.Command.State != StateRunning {
		t.Fatalf("live command was recovered or changed: %+v", status)
	}
	state, _ := first.get(started.Command.ID)
	state.mu.Lock()
	pid := state.cmd.Process.Pid
	state.mu.Unlock()
	if err = syscall.Kill(pid, 0); err != nil {
		t.Fatalf("live process was disturbed: %v", err)
	}
}

func TestPersistenceFailureMakesManagerUnhealthyAndRejectsStarts(t *testing.T) {
	persistence := &faultPersistence{delegate: diskPersistence{}, failPersistAt: 1}
	manager, err := NewManager(Config{
		StateDir:     t.TempDir(),
		WorkspaceDir: t.TempDir(),
		ChildUID:     -1,
		ChildGID:     -1,
		Persistence:  persistence,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if response := manager.Start(startRequest("first", "/bin/true")); response.Code != "UNHEALTHY" {
		t.Fatalf("first failure: %+v", response)
	}
	if response := manager.Start(startRequest("second", "/bin/true")); response.Code != "UNHEALTHY" {
		t.Fatalf("new start was not failed closed: %+v", response)
	}
	if response := NewServer(manager, "unused").Dispatch(context.Background(), Request{Version: ProtocolVersion, Operation: "health"}); response.Code != "UNHEALTHY" {
		t.Fatalf("health: %+v", response)
	}
}

func TestTerminalPersistenceFailureNeverClaimsDurableExit(t *testing.T) {
	persistence := &faultPersistence{delegate: diskPersistence{}, failPersistAt: 3}
	manager, err := NewManager(Config{
		StateDir:     t.TempDir(),
		WorkspaceDir: t.TempDir(),
		ChildUID:     -1,
		ChildGID:     -1,
		Persistence:  persistence,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	started := manager.Start(startRequest("terminal-fault", "/bin/sh", "-c", "true"))
	if !started.OK {
		t.Fatalf("start: %+v", started)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if response, unhealthy := manager.unhealthyResponse(); unhealthy {
			if response.Code != "UNHEALTHY" {
				t.Fatalf("health: %+v", response)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal persistence failure was not observed")
		}
		time.Sleep(time.Millisecond)
	}
	status := manager.Status(started.Command.ID)
	if !status.OK || status.Command.State.Terminal() {
		t.Fatalf("volatile terminal state was claimed durable: %+v", status)
	}
	if wait := manager.Wait(context.Background(), started.Command.ID, time.Second); wait.Code != "UNHEALTHY" {
		t.Fatalf("wait: %+v", wait)
	}
}

func TestSpoolPersistenceFailureFailsClosed(t *testing.T) {
	persistence := &faultPersistence{delegate: diskPersistence{}, failAppend: true}
	manager, err := NewManager(Config{
		StateDir:     t.TempDir(),
		WorkspaceDir: t.TempDir(),
		ChildUID:     -1,
		ChildGID:     -1,
		Persistence:  persistence,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	started := manager.Start(startRequest("spool-fault", "/bin/sh", "-c", "printf output; sleep 30"))
	if !started.OK {
		t.Fatalf("start: %+v", started)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if response, unhealthy := manager.unhealthyResponse(); unhealthy {
			if response.Code != "UNHEALTHY" {
				t.Fatalf("health: %+v", response)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("spool persistence failure was not observed")
		}
		time.Sleep(time.Millisecond)
	}
	if response := manager.Start(startRequest("after-spool-fault", "/bin/true")); response.Code != "UNHEALTHY" {
		t.Fatalf("new start was accepted: %+v", response)
	}
}

func TestBlockedStdinDoesNotBlockSignal(t *testing.T) {
	manager := testManager(t)
	started := manager.Start(startRequest("blocked-stdin", "/bin/sh", "-c", "sleep 30"))
	if !started.OK {
		t.Fatalf("start: %+v", started)
	}
	writes := make(chan Response, 2)
	payload := bytes.Repeat([]byte{'x'}, MaxStdinBytes)
	for range 2 {
		go func() { writes <- manager.Stdin(started.Command.ID, payload) }()
	}
	time.Sleep(100 * time.Millisecond)
	begin := time.Now()
	if response := manager.Signal(started.Command.ID, "KILL"); !response.OK {
		t.Fatalf("signal: %+v", response)
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("signal waited behind stdin write for %v", elapsed)
	}
	for range 2 {
		select {
		case <-writes:
		case <-time.After(2 * time.Second):
			t.Fatal("blocked stdin write was not interrupted")
		}
	}
	if wait := manager.Wait(context.Background(), started.Command.ID, 5*time.Second); !wait.OK {
		t.Fatalf("wait: %+v", wait)
	}
}
