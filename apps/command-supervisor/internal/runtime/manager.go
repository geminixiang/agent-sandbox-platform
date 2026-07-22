package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	metadataMax         = 8 << 10
	diskOverheadReserve = 64 << 10
)

type Config struct {
	StateDir     string
	WorkspaceDir string
	ChildUID     int
	ChildGID     int
	RequireRoot  bool
	Persistence  Persistence
}

type processState struct {
	record      Command
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	containment commandContainment
	mu          sync.Mutex
	stdinWrite  sync.Mutex
	changed     chan struct{}
}

type Manager struct {
	cfg         Config
	lock        *StateLock
	persistence Persistence
	mu          sync.RWMutex
	commands    map[string]*processState
	requests    map[string]string
	healthMu    sync.RWMutex
	unhealthy   error
}

func NewManager(cfg Config) (_ *Manager, err error) {
	if cfg.StateDir == "" {
		return nil, errors.New("state directory is required")
	}
	if cfg.WorkspaceDir == "" {
		cfg.WorkspaceDir = "/workspace"
	}
	if !filepath.IsAbs(cfg.WorkspaceDir) {
		return nil, errors.New("workspace directory must be absolute")
	}
	if cfg.RequireRoot && os.Geteuid() != 0 {
		return nil, errors.New("supervisor must run as root")
	}

	// State ownership is acquired before commands are inspected, recovered,
	// truncated, or otherwise mutated. NewManager is the only construction seam.
	lock, err := AcquireStateLock(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	commandsDir := filepath.Join(cfg.StateDir, "commands")
	if err = os.Chmod(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(commandsDir, 0o700); err != nil {
		return nil, err
	}
	if err = os.Chmod(commandsDir, 0o700); err != nil {
		return nil, err
	}
	if cfg.RequireRoot {
		if err = ensureRootOwned(cfg.StateDir); err != nil {
			return nil, err
		}
		if err = ensureRootOwned(commandsDir); err != nil {
			return nil, err
		}
	}
	statePath, err := filepath.EvalSymlinks(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	workspacePath, err := filepath.EvalSymlinks(cfg.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	if pathContains(statePath, workspacePath) || pathContains(workspacePath, statePath) {
		return nil, errors.New("supervisor state must be separate from /workspace")
	}
	persistence := cfg.Persistence
	if persistence == nil {
		persistence = diskPersistence{}
	}
	m := &Manager{cfg: cfg, lock: lock, persistence: persistence, commands: make(map[string]*processState), requests: make(map[string]string)}
	if err = m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) Close() error {
	if m.lock == nil {
		return nil
	}
	err := m.lock.Close()
	m.lock = nil
	return err
}

func pathContains(parent, child string) bool {
	return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

func (m *Manager) load() error {
	root := filepath.Join(m.cfg.StateDir, "commands")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) > MaxCommands {
		return errors.New("persisted command record limit exceeded")
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name(), "metadata.json"))
		if err != nil {
			return fmt.Errorf("read command %s: %w", entry.Name(), err)
		}
		var record Command
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("decode command %s: %w", entry.Name(), err)
		}
		if record.ID != entry.Name() || len(record.ID) != 48 {
			return fmt.Errorf("command directory %s has invalid identity", entry.Name())
		}
		if record.NextSeq == 0 {
			return fmt.Errorf("command %s has invalid sequence", entry.Name())
		}
		if !record.State.Terminal() && record.State != StateStarting && record.State != StateRunning {
			return fmt.Errorf("command %s has invalid state", entry.Name())
		}
		if _, err := hex.DecodeString(record.ID); err != nil {
			return fmt.Errorf("command directory %s has invalid identity", entry.Name())
		}
		events, err := readEvents(filepath.Join(root, entry.Name(), "spool"))
		if err != nil {
			return fmt.Errorf("read command %s events: %w", entry.Name(), err)
		}
		dirty := false
		if len(events) == 0 && record.NextSeq != 1 {
			return fmt.Errorf("command %s metadata references missing output", entry.Name())
		}
		if len(events) > 0 {
			lastNext := events[len(events)-1].Seq + 1
			if record.NextSeq > lastNext {
				return fmt.Errorf("command %s metadata is ahead of its spool", entry.Name())
			}
			if record.NextSeq < lastNext {
				// A crash can occur after fsyncing an event and before atomically
				// advancing metadata. The versioned spool is authoritative here.
				record.NextSeq = lastNext
				dirty = true
			}
		}
		if record.State == StateStarting || record.State == StateRunning {
			record.State = StateLost
			record.FinishedAt = time.Now().UTC()
			// Persisted process identifiers are deliberately neither loaded nor signalled.
			dirty = true
		}
		if dirty {
			if err := m.persist(&record); err != nil {
				return err
			}
		}
		state := &processState{record: record, changed: make(chan struct{})}
		m.commands[record.ID] = state
		if record.RequestID != "" {
			if _, exists := m.requests[record.RequestID]; exists {
				return fmt.Errorf("duplicate persisted requestId for command %s", entry.Name())
			}
			m.requests[record.RequestID] = record.ID
		}
	}
	return nil
}

func (m *Manager) commandDir(id string) string { return filepath.Join(m.cfg.StateDir, "commands", id) }

func (m *Manager) persist(record *Command) error {
	return m.persistence.PersistCommand(m.cfg.StateDir, record)
}

func (m *Manager) markUnhealthy(err error) {
	if err == nil {
		return
	}
	m.healthMu.Lock()
	if m.unhealthy == nil {
		m.unhealthy = err
	}
	m.healthMu.Unlock()
}

func (m *Manager) unhealthyResponse() (Response, bool) {
	m.healthMu.RLock()
	unhealthy := m.unhealthy != nil
	m.healthMu.RUnlock()
	if unhealthy {
		return failure("UNHEALTHY", "durable state persistence is unavailable"), true
	}
	return Response{}, false
}

func (m *Manager) persistTransition(record *Command) bool {
	if err := m.persist(record); err != nil {
		m.markUnhealthy(err)
		return false
	}
	return true
}

func normalize(req Request) (Command, error) {
	if req.RequestID == "" || len(req.RequestID) > 200 {
		return Command{}, errors.New("requestId must contain 1..200 characters")
	}
	if len(req.Argv) == 0 || len(req.Argv) > 256 {
		return Command{}, errors.New("argv must contain 1..256 entries")
	}
	for _, arg := range req.Argv {
		if len(arg) > 64<<10 || strings.IndexByte(arg, 0) >= 0 {
			return Command{}, errors.New("invalid argv entry")
		}
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = "/workspace"
	}
	cwd = filepath.Clean(cwd)
	if cwd != "/workspace" && !strings.HasPrefix(cwd, "/workspace/") {
		return Command{}, errors.New("cwd must be /workspace or beneath it")
	}
	keys := make([]string, 0, len(req.Env))
	if len(req.Env) > 256 {
		return Command{}, errors.New("env has too many entries")
	}
	for key, value := range req.Env {
		if key == "" || len(key) > 256 || len(value) > 64<<10 || strings.ContainsAny(key, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return Command{}, errors.New("invalid env entry")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+req.Env[key])
	}
	spec, _ := json.Marshal(struct {
		Argv []string `json:"argv"`
		Cwd  string   `json:"cwd"`
		Env  []string `json:"env"`
	}{req.Argv, cwd, env})
	if len(spec) > MaxRequestBytes {
		return Command{}, errors.New("normalized command specification exceeds request limit")
	}
	hash := sha256.Sum256(spec)
	return Command{RequestID: req.RequestID, Argv: append([]string(nil), req.Argv...), Cwd: cwd, Env: env, SpecHash: hex.EncodeToString(hash[:])}, nil
}

func randomID() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (m *Manager) commandCwd(cwd string) (string, error) {
	root, err := filepath.EvalSymlinks(m.cfg.WorkspaceDir)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	relative := strings.TrimPrefix(cwd, "/workspace")
	target, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", errors.New("cwd resolves outside /workspace")
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("cwd is not a directory")
	}
	return target, nil
}

func (m *Manager) Start(req Request) Response {
	if response, unhealthy := m.unhealthyResponse(); unhealthy {
		return response
	}
	record, err := normalize(req)
	if err != nil {
		return failure("INVALID_ARGUMENT", err.Error())
	}
	commandCwd, err := m.commandCwd(record.Cwd)
	if err != nil {
		return failure("INVALID_ARGUMENT", err.Error())
	}
	m.mu.Lock()
	if id, ok := m.requests[record.RequestID]; ok {
		state := m.commands[id]
		state.mu.Lock()
		existing := state.record
		state.mu.Unlock()
		m.mu.Unlock()
		if existing.SpecHash != record.SpecHash {
			return failure("IDEMPOTENCY_CONFLICT", "requestId was already used with a different command specification")
		}
		response := success()
		response.Command = &existing
		return response
	}
	if len(m.commands) >= MaxCommands {
		m.mu.Unlock()
		return failure("COMMAND_LIMIT", "command record limit reached")
	}
	id, err := randomID()
	if err != nil {
		m.mu.Unlock()
		return failure("INTERNAL", "could not generate command id")
	}
	now := time.Now().UTC()
	record.ID, record.State, record.CreatedAt, record.NextSeq = id, StateStarting, now, 1
	state := &processState{record: record, containment: newCommandContainment(), changed: make(chan struct{})}
	m.commands[id], m.requests[record.RequestID] = state, id
	if !m.persistTransition(&record) {
		delete(m.commands, id)
		delete(m.requests, record.RequestID)
		m.mu.Unlock()
		response, _ := m.unhealthyResponse()
		return response
	}
	m.mu.Unlock()

	cmd := exec.Command(record.Argv[0], record.Argv[1:]...)
	cmd.Dir, cmd.Env = commandCwd, append([]string(nil), record.Env...)
	configureChild(cmd, m.cfg.ChildUID, m.cfg.ChildGID, state.containment)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return m.startFailed(state, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdout.Close()
		return m.startFailed(state, err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		stdout.Close()
		stderr.Close()
		return m.startFailed(state, err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return m.startFailed(state, err)
	}
	if err := state.containment.Started(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return m.startFailed(state, err)
	}
	state.mu.Lock()
	state.cmd, state.stdin = cmd, stdin
	started := cloneCommand(state.record)
	started.State, started.StartedAt = StateRunning, time.Now().UTC()
	persisted := m.persistTransition(&started)
	if persisted {
		state.record = started
		state.notifyLocked()
	}
	state.mu.Unlock()
	go m.collect(state, stdout, stderr)
	if !persisted {
		_ = stdin.Close()
		_ = state.containment.Signal(syscall.SIGKILL)
		response, _ := m.unhealthyResponse()
		return response
	}
	response := success()
	response.Command = &started
	return response
}

func (m *Manager) startFailed(state *processState, cause error) Response {
	state.mu.Lock()
	lost := cloneCommand(state.record)
	lost.State, lost.FinishedAt = StateLost, time.Now().UTC()
	if !m.persistTransition(&lost) {
		state.notifyLocked()
		state.mu.Unlock()
		response, _ := m.unhealthyResponse()
		return response
	}
	state.record = lost
	state.notifyLocked()
	state.mu.Unlock()
	return failure("START_FAILED", cause.Error())
}

type outputChunk struct {
	stream string
	data   []byte
	eof    bool
}

func (m *Manager) collect(state *processState, stdout, stderr io.ReadCloser) {
	chunks := make(chan outputChunk, 4)
	read := func(stream string, input io.ReadCloser) {
		defer input.Close()
		for {
			buffer := make([]byte, MaxEventBytes)
			n, err := input.Read(buffer)
			if n > 0 {
				chunks <- outputChunk{stream: stream, data: buffer[:n]}
			}
			if err != nil {
				chunks <- outputChunk{stream: stream, eof: true}
				return
			}
		}
	}
	go read("stdout", stdout)
	go read("stderr", stderr)
	eofs := 0
	spoolFailed := false
	for eofs < 2 {
		chunk := <-chunks
		if chunk.eof {
			eofs++
			continue
		}
		state.mu.Lock()
		if !spoolFailed {
			event := Event{Seq: state.record.NextSeq, Stream: chunk.stream, Data: append([]byte(nil), chunk.data...)}
			if err := m.persistence.AppendEvent(m.cfg.StateDir, state.record.ID, event, MaxSpoolBytes-diskOverheadReserve); err != nil {
				spoolFailed = true
				m.markUnhealthy(err)
				_ = state.containment.Signal(syscall.SIGKILL)
			} else {
				advanced := cloneCommand(state.record)
				advanced.NextSeq++
				if m.persistTransition(&advanced) {
					state.record = advanced
					state.notifyLocked()
				} else {
					spoolFailed = true
					_ = state.containment.Signal(syscall.SIGKILL)
				}
			}
		}
		state.mu.Unlock()
	}
	// Wait only after both readers reach EOF. Calling Wait concurrently with
	// reads can close os/exec pipes before all output has been observed.
	waitErr := state.cmd.Wait()
	state.mu.Lock()
	stdin := state.stdin
	state.stdin = nil
	terminal := cloneCommand(state.record)
	terminal.FinishedAt = time.Now().UTC()
	if spoolFailed {
		terminal.State = StateLost
	} else if status, ok := state.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		terminal.State, terminal.Signal = StateSignaled, status.Signal().String()
	} else {
		terminal.State = StateExited
		code := state.cmd.ProcessState.ExitCode()
		terminal.ExitCode = &code
		_ = waitErr
	}
	if m.persistTransition(&terminal) {
		state.record = terminal
	}
	state.notifyLocked()
	state.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
}

func (s *processState) notifyLocked() { close(s.changed); s.changed = make(chan struct{}) }

func (m *Manager) get(id string) (*processState, Response) {
	m.mu.RLock()
	state := m.commands[id]
	m.mu.RUnlock()
	if state == nil {
		return nil, failure("NOT_FOUND", "command not found")
	}
	return state, Response{}
}

func cloneCommand(command Command) Command {
	command.Argv = append([]string(nil), command.Argv...)
	command.Env = append([]string(nil), command.Env...)
	return command
}

func (m *Manager) Status(id string) Response {
	state, fail := m.get(id)
	if state == nil {
		return fail
	}
	state.mu.Lock()
	command := cloneCommand(state.record)
	state.mu.Unlock()
	response := success()
	response.Command = &command
	return response
}

func (m *Manager) List() Response {
	m.mu.RLock()
	states := make([]*processState, 0, len(m.commands))
	for _, state := range m.commands {
		states = append(states, state)
	}
	m.mu.RUnlock()
	commands := make([]Command, 0, len(states))
	for _, state := range states {
		state.mu.Lock()
		commands = append(commands, cloneCommand(state.record))
		state.mu.Unlock()
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].CreatedAt.Before(commands[j].CreatedAt) })
	response := success()
	response.Commands = commands
	return response
}

func (m *Manager) Connect(id string, after uint64) Response {
	state, fail := m.get(id)
	if state == nil {
		return fail
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	generated := state.record.NextSeq - 1
	if after > generated {
		return failure("INVALID_CURSOR", "cursor is ahead of generated output")
	}
	retainedEvents, err := readEvents(filepath.Join(m.commandDir(state.record.ID), "spool"))
	if err != nil {
		return failure("INTERNAL", "could not read retained output")
	}
	retained := state.record.NextSeq
	if len(retainedEvents) > 0 {
		retained = retainedEvents[0].Seq
	}
	if after+1 < retained {
		return failure("CURSOR_EXPIRED", "cursor precedes retained output")
	}
	events := make([]Event, 0)
	for _, event := range retainedEvents {
		if event.Seq > after {
			event.Data = append([]byte(nil), event.Data...)
			events = append(events, event)
		}
	}
	response := success()
	response.Events = events
	response.NextCursor = generated
	command := cloneCommand(state.record)
	response.Command = &command
	return response
}

func (m *Manager) Stdin(id string, data []byte) Response {
	if len(data) > MaxStdinBytes {
		return failure("REQUEST_TOO_LARGE", "stdin payload exceeds limit")
	}
	state, fail := m.get(id)
	if state == nil {
		return fail
	}
	// Serialize writes without holding the lifecycle mutex. Closing the pipe
	// from CloseStdin, Signal, or collect interrupts a blocked os.File.Write.
	state.stdinWrite.Lock()
	defer state.stdinWrite.Unlock()
	state.mu.Lock()
	stdin := state.stdin
	running := state.record.State == StateRunning
	state.mu.Unlock()
	if !running || stdin == nil {
		return failure("NOT_RUNNING", "command stdin is not open")
	}
	if _, err := stdin.Write(data); err != nil {
		return failure("IO_ERROR", err.Error())
	}
	return success()
}

func (m *Manager) CloseStdin(id string) Response {
	state, fail := m.get(id)
	if state == nil {
		return fail
	}
	state.mu.Lock()
	stdin := state.stdin
	state.stdin = nil
	state.mu.Unlock()
	if stdin == nil {
		return failure("STDIN_CLOSED", "command stdin is already closed")
	}
	if err := stdin.Close(); err != nil {
		return failure("IO_ERROR", err.Error())
	}
	return success()
}

func (m *Manager) Signal(id, name string) Response {
	state, fail := m.get(id)
	if state == nil {
		return fail
	}
	var signal syscall.Signal
	switch name {
	case "TERM":
		signal = syscall.SIGTERM
	case "KILL":
		signal = syscall.SIGKILL
	default:
		return failure("INVALID_SIGNAL", "signal must be TERM or KILL")
	}
	state.mu.Lock()
	if state.record.State != StateRunning || state.containment == nil {
		state.mu.Unlock()
		return failure("NOT_RUNNING", "command is not running")
	}
	stdin := state.stdin
	state.stdin = nil
	containment := state.containment
	state.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if err := containment.Signal(signal); err != nil {
		return failure("SIGNAL_FAILED", err.Error())
	}
	return success()
}

func (m *Manager) KillAll() Response {
	m.mu.RLock()
	ids := make([]string, 0, len(m.commands))
	for id := range m.commands {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	var firstFailure *Response
	for _, id := range ids {
		response := m.Signal(id, "KILL")
		if !response.OK && response.Code != "NOT_RUNNING" && firstFailure == nil {
			copy := response
			firstFailure = &copy
		}
	}
	if firstFailure != nil {
		return *firstFailure
	}
	return success()
}

func (m *Manager) Wait(ctx context.Context, id string, timeout time.Duration) Response {
	state, fail := m.get(id)
	if state == nil {
		return fail
	}
	if timeout <= 0 || timeout > 5*time.Minute {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if response, unhealthy := m.unhealthyResponse(); unhealthy {
			return response
		}
		state.mu.Lock()
		command := cloneCommand(state.record)
		changed := state.changed
		state.mu.Unlock()
		if command.State.Terminal() {
			response := success()
			response.Command = &command
			return response
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return failure("WAIT_TIMEOUT", "command did not become terminal before timeout")
		}
	}
}
