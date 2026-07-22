//go:build linux

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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func linuxRootManager(t *testing.T) *Manager {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires Linux root to verify setuid, capabilities, and process-group signalling")
	}
	workspace, err := os.MkdirTemp("/tmp", "asp-supervisor-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(workspace, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(workspace) })
	state, err := os.MkdirTemp("/tmp", "asp-supervisor-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	manager, err := NewManager(Config{StateDir: state, WorkspaceDir: workspace, ChildUID: 10001, ChildGID: 10001, RequireRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.KillAll()
		_ = manager.Close()
	})
	return manager
}

func commandOutput(t *testing.T, manager *Manager, id string) string {
	t.Helper()
	wait := manager.Wait(context.Background(), id, 10*time.Second)
	if !wait.OK {
		t.Fatalf("wait: %+v", wait)
	}
	result := manager.Connect(id, 0)
	if !result.OK {
		t.Fatalf("connect: %+v", result)
	}
	var output strings.Builder
	for _, event := range result.Events {
		output.Write(event.Data)
	}
	return output.String()
}

func TestLinuxChildIdentityCapabilitiesAndNoNewPrivileges(t *testing.T) {
	manager := linuxRootManager(t)
	response := manager.Start(startRequest("identity", "/bin/sh", "-c", `printf 'uid=%s gid=%s\n' "$(id -u)" "$(id -g)"; grep -E '^(Groups|Cap(Inh|Prm|Eff|Amb)|NoNewPrivs):' /proc/self/status`))
	if !response.OK {
		t.Fatalf("start: %+v", response)
	}
	output := commandOutput(t, manager, response.Command.ID)
	for _, want := range []string{"uid=10001 gid=10001", "Groups:\t10001", "CapInh:\t0000000000000000", "CapPrm:\t0000000000000000", "CapEff:\t0000000000000000", "CapAmb:\t0000000000000000", "NoNewPrivs:\t1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
}

func TestLinuxTrampolineClearsInjectedInheritableCapability(t *testing.T) {
	if os.Getenv("ASP_INHERITABLE_HELPER") == "1" {
		header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		data := [2]unix.CapUserData{}
		if err := unix.Capget(&header, &data[0]); err != nil {
			t.Fatal(err)
		}
		injected := false
		for index := range data {
			if data[index].Permitted == 0 {
				continue
			}
			bit := uint32(1) << uint32(bitsTrailingZeros32(data[index].Permitted))
			data[index].Inheritable |= bit
			injected = true
			break
		}
		if !injected {
			fmt.Println("ASP_INHERITABLE_UNAVAILABLE")
			return
		}
		if err := unix.Capset(&header, &data[0]); err != nil {
			fmt.Println("ASP_INHERITABLE_UNAVAILABLE")
			return
		}
		manager := linuxRootManager(t)
		response := manager.Start(startRequest("inheritable", "/bin/sh", "-c", "grep '^CapInh:' /proc/self/status"))
		if !response.OK {
			t.Fatalf("start: %+v", response)
		}
		if output := commandOutput(t, manager, response.Command.ID); !strings.Contains(output, "CapInh:\t0000000000000000") {
			t.Fatalf("inheritable capability survived trampoline: %s", output)
		}
		fmt.Println("ASP_INHERITABLE_CLEARED")
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires Linux root with at least one permitted capability")
	}
	helper, err := os.CreateTemp("/tmp", "asp-inheritable-test-helper-")
	if err != nil {
		t.Fatal(err)
	}
	helperPath := helper.Name()
	_ = helper.Close()
	defer os.Remove(helperPath)
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(helperPath, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(helperPath, "-test.run=TestLinuxTrampolineClearsInjectedInheritableCapability")
	cmd.Env = append(os.Environ(), "ASP_INHERITABLE_HELPER=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("inheritable helper: %v: %s", err, output)
	}
	if strings.Contains(string(output), "ASP_INHERITABLE_UNAVAILABLE") {
		t.Skip("kernel capability set did not permit injecting an inheritable capability")
	}
	if !strings.Contains(string(output), "ASP_INHERITABLE_CLEARED") {
		t.Fatalf("helper did not execute capability gate: %s", output)
	}
}

func bitsTrailingZeros32(value uint32) int {
	for bit := 0; bit < 32; bit++ {
		if value&(uint32(1)<<uint32(bit)) != 0 {
			return bit
		}
	}
	return 32
}

func TestLinuxKillCapabilityIsRequiredAcrossUIDs(t *testing.T) {
	manager := linuxRootManager(t)
	response := manager.Start(startRequest("cap-kill", "/bin/sh", "-c", "sleep 60"))
	if !response.OK {
		t.Fatalf("start: %+v", response)
	}
	state, _ := manager.get(response.Command.ID)
	state.mu.Lock()
	pid := state.cmd.Process.Pid
	state.mu.Unlock()

	helper, err := os.CreateTemp("/tmp", "asp-cap-test-helper-")
	if err != nil {
		t.Fatal(err)
	}
	helperPath := helper.Name()
	helper.Close()
	t.Cleanup(func() { os.Remove(helperPath) })
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(helperPath, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(includeKill bool) {
		cmd := exec.Command(helperPath, "-test.run=TestLinuxCapabilitySignalHelper")
		cmd.Env = append(os.Environ(), "ASP_CAP_TEST_PID="+strconv.Itoa(pid), fmt.Sprintf("ASP_CAP_TEST_KILL=%t", includeKill))
		if output, runErr := cmd.CombinedOutput(); runErr != nil {
			t.Fatalf("capability helper (KILL=%t): %v: %s", includeKill, runErr, output)
		}
	}
	run(false)
	run(true)
	if cleanup := manager.Signal(response.Command.ID, "KILL"); !cleanup.OK {
		t.Fatalf("cleanup: %+v", cleanup)
	}
	_ = manager.Wait(context.Background(), response.Command.ID, 5*time.Second)
}

func TestLinuxCapabilitySignalHelper(t *testing.T) {
	pidText := os.Getenv("ASP_CAP_TEST_PID")
	if pidText == "" {
		t.Skip("helper only")
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatal(err)
	}
	mask := uint32(1<<unix.CAP_SETUID | 1<<unix.CAP_SETGID)
	includeKill := os.Getenv("ASP_CAP_TEST_KILL") == "true"
	if includeKill {
		mask |= 1 << unix.CAP_KILL
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{{Effective: mask, Permitted: mask}}
	if err = unix.Capset(&header, &data[0]); err != nil {
		t.Fatal(err)
	}
	err = syscall.Kill(pid, 0)
	if includeKill && err != nil {
		t.Fatalf("KILL capability did not authorize signal: %v", err)
	}
	if !includeKill && !errors.Is(err, syscall.EPERM) {
		t.Fatalf("SETUID/SETGID-only signal = %v, want EPERM", err)
	}
}

func TestLinuxProcessGroupsSignalsAndKillAll(t *testing.T) {
	manager := linuxRootManager(t)
	start := func(id string) (string, int) {
		// The shell becomes a second ordinary sleep; there is no shell wait or
		// cleanup behavior that could mask whether the background descendant
		// received the process-group signal.
		response := manager.Start(startRequest(id, "/bin/sh", "-c", "sleep 60 & echo $!; exec sleep 60"))
		if !response.OK {
			t.Fatalf("start: %+v", response)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			connected := manager.Connect(response.Command.ID, 0)
			if connected.OK && len(connected.Events) > 0 {
				pid, err := strconv.Atoi(strings.TrimSpace(string(connected.Events[0].Data)))
				if err != nil {
					t.Fatalf("descendant PID: %v", err)
				}
				return response.Command.ID, pid
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("descendant did not start")
		return "", 0
	}
	assertGone := func(pid int) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("descendant %d survived process-group signal", pid)
	}
	termID, termChild := start("term")
	if response := manager.Signal(termID, "TERM"); !response.OK {
		t.Fatalf("TERM: %+v", response)
	}
	if wait := manager.Wait(context.Background(), termID, 5*time.Second); !wait.OK || wait.Command.State != StateSignaled {
		t.Fatalf("TERM wait: %+v", wait)
	}
	assertGone(termChild)
	killID, killChild := start("kill")
	if response := manager.Signal(killID, "KILL"); !response.OK {
		t.Fatalf("KILL: %+v", response)
	}
	if wait := manager.Wait(context.Background(), killID, 5*time.Second); !wait.OK || wait.Command.State != StateSignaled {
		t.Fatalf("KILL wait: %+v", wait)
	}
	assertGone(killChild)
	one, oneChild := start("all-1")
	two, twoChild := start("all-2")
	if response := manager.KillAll(); !response.OK {
		t.Fatalf("killAll: %+v", response)
	}
	for _, id := range []string{one, two} {
		if wait := manager.Wait(context.Background(), id, 5*time.Second); !wait.OK || !wait.Command.State.Terminal() {
			t.Fatalf("killAll wait: %+v", wait)
		}
	}
	assertGone(oneChild)
	assertGone(twoChild)
}

func TestLinuxSetsidEscapeIsExplicitProductionBlocker(t *testing.T) {
	if _, err := os.Stat("/usr/bin/setsid"); err != nil {
		t.Skip("setsid utility is unavailable")
	}
	manager := linuxRootManager(t)
	response := manager.Start(startRequest("setsid-escape", "/bin/sh", "-c", `/usr/bin/setsid /bin/sh -c 'echo $$ > escaped.pid; exec sleep 60' </dev/null >/dev/null 2>&1 & while [ ! -s escaped.pid ]; do sleep .01; done; cat escaped.pid; exec sleep 60`))
	if !response.OK {
		t.Fatalf("start: %+v", response)
	}
	var escapedPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connected := manager.Connect(response.Command.ID, 0)
		if connected.OK && len(connected.Events) > 0 {
			escapedPID, _ = strconv.Atoi(strings.TrimSpace(string(connected.Events[0].Data)))
			if escapedPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if escapedPID <= 0 {
		t.Fatal("setsid descendant did not report its PID")
	}
	defer syscall.Kill(escapedPID, syscall.SIGKILL)
	if result := manager.Signal(response.Command.ID, "KILL"); !result.OK {
		t.Fatalf("signal: %+v", result)
	}
	if wait := manager.Wait(context.Background(), response.Command.ID, 5*time.Second); !wait.OK {
		t.Fatalf("wait: %+v", wait)
	}
	if err := syscall.Kill(escapedPID, 0); err != nil {
		t.Fatalf("diagnostic setup did not demonstrate setsid escape: %v", err)
	}
	t.Skip("PRODUCTION BLOCKER: PGID signalling cannot kill descendants that call setsid; cgroup v2 containment is required")
}

func TestLinuxRecoveryNeverSignalsPersistedProcessIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires Linux root")
	}
	unrelated := exec.Command("/bin/sleep", "60")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unrelated.Process.Kill()
		_ = unrelated.Wait()
	}()

	stateDir, workspace := t.TempDir(), t.TempDir()
	commandID := strings.Repeat("cd", 24)
	commandDir := filepath.Join(stateDir, "commands", commandID)
	if err := os.MkdirAll(commandDir, 0o700); err != nil {
		t.Fatal(err)
	}
	record := Command{ID: commandID, RequestID: "restart", Argv: []string{"/bin/sleep", "60"}, Cwd: "/workspace", SpecHash: "hash", State: StateRunning, CreatedAt: time.Now(), NextSeq: 1}
	if err := atomicJSON(filepath.Join(commandDir, "metadata.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewManager(Config{StateDir: stateDir, WorkspaceDir: workspace, ChildUID: 10001, ChildGID: 10001, RequireRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if status := recovered.Status(commandID); !status.OK || status.Command.State != StateLost {
		t.Fatalf("recovery: %+v", status)
	}
	if err := syscall.Kill(unrelated.Process.Pid, 0); err != nil {
		t.Fatalf("recovery signalled a live process without owned runtime identity: %v", err)
	}
}

func TestLinuxSocketOwnershipSingletonAndUIDDenial(t *testing.T) {
	manager := linuxRootManager(t)
	socket := filepath.Join(manager.cfg.StateDir, "supervisor.sock")
	server := NewServer(manager, socket)
	listener, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %o", info.Mode().Perm())
	}
	if _, err = NewManager(manager.cfg); err == nil {
		t.Fatal("second supervisor acquired singleton state ownership")
	}

	helper, err := os.CreateTemp("/tmp", "asp-supervisor-test-helper-")
	if err != nil {
		t.Fatal(err)
	}
	helperPath := helper.Name()
	helper.Close()
	t.Cleanup(func() { os.Remove(helperPath) })
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(helperPath, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(helperPath, "-test.run=TestLinuxUID10001DialHelper")
	cmd.Env = append(os.Environ(), "ASP_TEST_SOCKET="+socket)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 10001, Gid: 10001, Groups: []uint32{}}}
	if err = cmd.Run(); err != nil {
		t.Fatalf("UID 10001 denial helper: %v", err)
	}

	// Make filesystem permissions permissive temporarily so this second helper
	// reaches accept(2); SO_PEERCRED must still reject its UID before dispatch.
	if err = os.Chmod(manager.cfg.StateDir, 0o711); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(socket, 0o666); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(helperPath, "-test.run=TestLinuxUIDPeerCredentialHelper")
	cmd.Env = append(os.Environ(), "ASP_TEST_SOCKET="+socket)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 10001, Gid: 10001, Groups: []uint32{}}}
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("SO_PEERCRED helper: %v: %s", runErr, output)
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLinuxUID10001DialHelper(t *testing.T) {
	socket := os.Getenv("ASP_TEST_SOCKET")
	if socket == "" {
		t.Skip("helper only")
	}
	if _, err := net.DialTimeout("unix", socket, time.Second); err == nil {
		t.Fatal("UID 10001 opened protected supervisor socket")
	}
}

func TestLinuxUIDPeerCredentialHelper(t *testing.T) {
	socket := os.Getenv("ASP_TEST_SOCKET")
	if socket == "" {
		t.Skip("helper only")
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatalf("permissive test socket was not reachable: %v", err)
	}
	defer conn.Close()
	request, _ := json.Marshal(Request{Version: ProtocolVersion, Operation: "health"})
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(request)))
	_, _ = conn.Write(header[:])
	_, _ = conn.Write(request)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = io.ReadFull(conn, header[:]); err == nil {
		t.Fatal("non-root peer passed SO_PEERCRED verification")
	}
}

func TestLinuxRootPeerCredentialAccepted(t *testing.T) {
	manager := linuxRootManager(t)
	socket := filepath.Join(manager.cfg.StateDir, "supervisor.sock")
	server := NewServer(manager, socket)
	listener, err := server.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	request, _ := json.Marshal(Request{Version: ProtocolVersion, Operation: "health"})
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(request)))
	_, _ = conn.Write(header[:])
	_, _ = conn.Write(request)
	if _, err = io.ReadFull(conn, header[:]); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err = io.ReadFull(conn, body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("response: %s", body)
	}
	conn.Close()
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLinuxIntegrationGateReason(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip(fmt.Sprintf("Linux integration gate skipped: euid=%d; run in a root-capable Linux test container", os.Geteuid()))
	}
}
