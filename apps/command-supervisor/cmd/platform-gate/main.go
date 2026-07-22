package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/geminixiang/agent-sandbox-platform/apps/command-supervisor/internal/gate"
	commandruntime "github.com/geminixiang/agent-sandbox-platform/apps/command-supervisor/internal/runtime"
)

const (
	defaultCtl        = "/usr/local/bin/agent-sandbox-ctl"
	defaultSupervisor = "/usr/local/bin/agent-sandbox-supervisor"
	defaultStateDir   = "/run/agent-sandbox-supervisor"
)

type runner struct {
	ctl        string
	supervisor string
	stateDir   string
	checks     []gate.Check
	escaped    []int
}

func main() {
	if handled, code := childSecurityProbeMode(); handled {
		os.Exit(code)
	}
	if handled, code := capabilityProbeMode(); handled {
		os.Exit(code)
	}
	ctl := flag.String("ctl", defaultCtl, "path to the ctl binary")
	supervisor := flag.String("supervisor", defaultSupervisor, "path to the supervisor binary")
	stateDir := flag.String("state-dir", defaultStateDir, "supervisor state directory")
	flag.Parse()

	r := &runner{ctl: *ctl, supervisor: *supervisor, stateDir: *stateDir}
	defer r.cleanup()
	r.check("health", r.checkHealth)
	r.check("root-only local client and protected state", r.checkRootAndState)
	r.check("singleton state ownership", r.checkSingletonOwnership)
	r.check("child identity and customer denial", r.checkChildSecurity)
	r.check("KILL capability is necessary and sufficient", r.checkKillCapability)
	r.check("idempotent start, list, and status", r.checkIdempotency)
	r.check("stdout/stderr ordering and fresh-ctl replay", r.checkReplay)
	r.check("bounded spool and expired cursor", r.checkSpool)
	r.check("stdin, close, and wait", r.checkStdin)
	r.check("TERM and KILL contain ordinary descendants after leader exit", r.checkSignals)
	r.check("killAll contains ordinary descendants", r.checkKillAll)
	setsidDetail, pgidContained, setsidErr := r.observeSetsidCompatibility()
	setsidStatus := gate.StatusPassed
	if setsidErr != nil {
		setsidStatus = gate.StatusFailed
		setsidDetail = detailWithError(setsidDetail, setsidErr)
	}
	r.checks = append(r.checks, gate.Check{Name: "new-session descendant compatibility", Status: setsidStatus, Detail: setsidDetail})

	cgroup, cgroupErr := inspectCgroupV2()
	cgroupStatus := gate.StatusPassed
	if cgroupErr != nil {
		cgroupStatus = gate.StatusFailed
		cgroup = detailWithError(cgroup, cgroupErr)
	}
	r.checks = append(r.checks, gate.Check{Name: "current cgroup v2 exposure", Status: cgroupStatus, Detail: cgroup})
	containmentAvailable, _ := cgroup["available"].(bool)
	// The prototype has no cgroup adapter. Availability is evidence for a
	// possible future mechanism, not evidence that this run contained the child.
	containmentStatus := gate.ClassifyContainment(pgidContained, false)
	containmentDetail := map[string]any{
		"adapter":                    "process-group",
		"pgidEscapeContained":        pgidContained,
		"cgroupContainmentAvailable": containmentAvailable,
		"cgroupAdapterEnabled":       false,
		"cgroupV2":                   cgroup,
	}
	if containmentStatus == gate.StatusPassed {
		containmentDetail["reason"] = "the enabled process-group mechanism contained the new-session descendant"
	} else {
		containmentDetail["reason"] = "production remains blocked because no enabled containment mechanism handled the new-session descendant"
	}
	r.checks = append(r.checks, gate.Check{Name: "production command containment", Status: containmentStatus, Detail: containmentDetail})

	report := gate.Report{
		SchemaVersion: 1,
		Test:          "command-supervisor-in-container-gate",
		Checks:        r.checks,
		Containment:   containmentDetail,
		Environment: map[string]any{
			"goos":         goruntime.GOOS,
			"goarch":       goruntime.GOARCH,
			"kernel":       kernelRelease(),
			"effectiveUID": os.Geteuid(),
		},
	}
	report.Status = gate.Summarize(report.Checks)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if report.Status == gate.StatusFailed {
		os.Exit(1)
	}
}

func (r *runner) check(name string, fn func() (map[string]any, error)) {
	detail, err := fn()
	status := gate.StatusPassed
	if err != nil {
		status = gate.StatusFailed
		detail = detailWithError(detail, err)
	}
	r.checks = append(r.checks, gate.Check{Name: name, Status: status, Detail: detail})
}

func detailWithError(detail map[string]any, err error) map[string]any {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["error"] = truncate(err.Error(), 512)
	return detail
}

func (r *runner) call(request commandruntime.Request) (commandruntime.Response, error) {
	request.Version = commandruntime.ProtocolVersion
	body, err := json.Marshal(request)
	if err != nil {
		return commandruntime.Response{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.ctl)
	cmd.Stdin = bytes.NewReader(body)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return commandruntime.Response{}, fmt.Errorf("fresh ctl invocation: %w: %s", err, truncate(string(output), 256))
	}
	var response commandruntime.Response
	if err = json.Unmarshal(output, &response); err != nil {
		return response, fmt.Errorf("decode ctl response: %w", err)
	}
	return response, nil
}

func (r *runner) ok(request commandruntime.Request) (commandruntime.Response, error) {
	response, err := r.call(request)
	if err != nil {
		return response, err
	}
	if !response.OK {
		return response, fmt.Errorf("operation %s failed with %s: %s", request.Operation, response.Code, response.Message)
	}
	return response, nil
}

func (r *runner) start(requestID string, argv ...string) (commandruntime.Response, error) {
	return r.ok(commandruntime.Request{Operation: "start", RequestID: requestID, Argv: argv, Cwd: "/workspace"})
}

func (r *runner) wait(id string) (commandruntime.Response, error) {
	return r.ok(commandruntime.Request{Operation: "wait", CommandID: id, TimeoutMS: 60000})
}

func (r *runner) connect(id string, after uint64) (commandruntime.Response, error) {
	return r.ok(commandruntime.Request{Operation: "connect", CommandID: id, After: after})
}

func (r *runner) output(id string) (string, []commandruntime.Event, error) {
	response, err := r.connect(id, 0)
	if err != nil {
		return "", nil, err
	}
	var output strings.Builder
	for _, event := range response.Events {
		output.Write(event.Data)
	}
	return output.String(), response.Events, nil
}

func (r *runner) firstPID(id string) (int, error) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, err := r.connect(id, 0)
		if err == nil && len(response.Events) > 0 {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(response.Events[0].Data)))
			if parseErr == nil && pid > 0 {
				return pid, nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, errors.New("command did not report a process identity")
}

func (r *runner) signal(id, signal string) error {
	_, err := r.ok(commandruntime.Request{Operation: "signal", CommandID: id, Signal: signal})
	return err
}

func (r *runner) cleanup() {
	_, _ = r.call(commandruntime.Request{Operation: "killAll"})
	for _, pid := range r.escaped {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func (r *runner) forgetEscaped(pid int) {
	for index, escapedPID := range r.escaped {
		if escapedPID == pid {
			r.escaped = append(r.escaped[:index], r.escaped[index+1:]...)
			return
		}
	}
}

func (r *runner) checkHealth() (map[string]any, error) {
	response, err := r.ok(commandruntime.Request{Operation: "health"})
	if err != nil {
		return nil, err
	}
	return map[string]any{"protocolVersion": response.Version, "freshCtl": true}, nil
}

func (r *runner) checkRootAndState() (map[string]any, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("gate must run as root, got euid %d", os.Geteuid())
	}
	stateInfo, err := os.Stat(r.stateDir)
	if err != nil {
		return nil, err
	}
	socketInfo, err := os.Stat(filepath.Join(r.stateDir, "supervisor.sock"))
	if err != nil {
		return nil, err
	}
	if stateInfo.Mode().Perm() != 0o700 || socketInfo.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("state/socket modes are %04o/%04o, want 0700/0600", stateInfo.Mode().Perm(), socketInfo.Mode().Perm())
	}
	return map[string]any{"stateMode": "0700", "socketMode": "0600", "peer": "root accepted"}, nil
}

func (r *runner) checkSingletonOwnership() (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.supervisor, "--state-dir", r.stateDir)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return nil, errors.New("second supervisor did not reject singleton ownership promptly")
	}
	if err == nil || !strings.Contains(string(output), "another supervisor owns state directory") {
		return map[string]any{"output": truncate(string(output), 128)}, errors.New("second supervisor did not report the held state lock")
	}
	if _, err = r.ok(commandruntime.Request{Operation: "health"}); err != nil {
		return nil, fmt.Errorf("original supervisor was disturbed: %w", err)
	}
	return map[string]any{"secondOwnerRejected": true, "originalOwnerHealthy": true}, nil
}

func (r *runner) checkChildSecurity() (map[string]any, error) {
	started, err := r.start("gate-child-security", "/usr/local/bin/agent-sandbox-platform-gate", "child-security-probe", r.stateDir)
	if err != nil {
		return nil, err
	}
	waited, err := r.wait(started.Command.ID)
	if err != nil {
		return nil, err
	}
	if waited.Command.State != commandruntime.StateExited || waited.Command.ExitCode == nil || *waited.Command.ExitCode != 0 {
		return nil, fmt.Errorf("security probe did not exit successfully")
	}
	output, _, err := r.output(started.Command.ID)
	if err != nil || output != "secure" {
		return nil, fmt.Errorf("security probe output mismatch")
	}
	return map[string]any{"uid": 10001, "gid": 10001, "capabilities": "zero", "noNewPrivileges": true, "customerSocketAccess": "denied"}, nil
}

func (r *runner) checkKillCapability() (map[string]any, error) {
	started, err := r.start("gate-capability-target", "/bin/sh", "-c", "echo $$; exec sleep 60")
	if err != nil {
		return nil, err
	}
	pid, err := r.firstPID(started.Command.ID)
	if err != nil {
		return nil, err
	}
	for _, includeKill := range []bool{false, true} {
		cmd := exec.Command("/proc/self/exe", "capability-probe", strconv.Itoa(pid), strconv.FormatBool(includeKill))
		if output, runErr := cmd.CombinedOutput(); runErr != nil {
			return nil, fmt.Errorf("capability probe includeKill=%t: %w: %s", includeKill, runErr, truncate(string(output), 128))
		}
	}
	if err = r.signal(started.Command.ID, "KILL"); err != nil {
		return nil, err
	}
	_, _ = r.wait(started.Command.ID)
	return map[string]any{"setuidSetgidOnly": "EPERM", "withKill": "authorized"}, nil
}

func (r *runner) checkIdempotency() (map[string]any, error) {
	marker := "/workspace/idempotency-marker"
	_ = os.Remove(marker)
	argv := []string{"/bin/sh", "-c", "printf x >> /workspace/idempotency-marker"}
	first, err := r.start("gate-idempotency", argv...)
	if err != nil {
		return nil, err
	}
	second, err := r.start("gate-idempotency", argv...)
	if err != nil {
		return nil, err
	}
	if first.Command.ID != second.Command.ID {
		return nil, errors.New("repeated normalized start returned a different command ID")
	}
	conflict, err := r.call(commandruntime.Request{Operation: "start", RequestID: "gate-idempotency", Argv: []string{"/bin/true"}, Cwd: "/workspace"})
	if err != nil || conflict.OK || conflict.Code != "IDEMPOTENCY_CONFLICT" {
		return nil, errors.New("different specification did not return IDEMPOTENCY_CONFLICT")
	}
	if _, err = r.wait(first.Command.ID); err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(marker)
	if err != nil || string(contents) != "x" {
		return nil, errors.New("idempotent start executed more or less than once")
	}
	status, err := r.ok(commandruntime.Request{Operation: "status", CommandID: first.Command.ID})
	if err != nil || status.Command.ID != first.Command.ID {
		return nil, errors.New("status did not return the stable command ID")
	}
	list, err := r.ok(commandruntime.Request{Operation: "list"})
	if err != nil {
		return nil, err
	}
	found := false
	for _, command := range list.Commands {
		found = found || command.ID == first.Command.ID
	}
	if !found {
		return nil, errors.New("list omitted the command")
	}
	return map[string]any{"stableCommandID": true, "duplicateExecutionPrevented": true, "conflictDetected": true}, nil
}

func (r *runner) checkReplay() (map[string]any, error) {
	script := `import os,time
os.write(1,b"out-1\n"); time.sleep(.03)
os.write(2,b"err-1\n"); time.sleep(.03)
os.write(1,b"out-2\n")`
	started, err := r.start("gate-replay", "/usr/local/bin/python3", "-c", script)
	if err != nil {
		return nil, err
	}
	if _, err = r.wait(started.Command.ID); err != nil {
		return nil, err
	}
	first, err := r.connect(started.Command.ID, 0)
	if err != nil {
		return nil, err
	}
	second, err := r.connect(started.Command.ID, 0)
	if err != nil {
		return nil, err
	}
	firstJSON, _ := json.Marshal(first.Events)
	secondJSON, _ := json.Marshal(second.Events)
	if !bytes.Equal(firstJSON, secondJSON) || len(first.Events) < 3 {
		return nil, errors.New("fresh ctl replay was incomplete or unstable")
	}
	streams := map[string]bool{}
	var previous uint64
	var replayed strings.Builder
	for _, event := range first.Events {
		if event.Seq <= previous {
			return nil, errors.New("event sequences were not strictly increasing")
		}
		previous = event.Seq
		streams[event.Stream] = true
		fmt.Fprintf(&replayed, "%s:%s", event.Stream, event.Data)
	}
	if !streams["stdout"] || !streams["stderr"] {
		return nil, errors.New("replay did not preserve both streams")
	}
	if replayed.String() != "stdout:out-1\nstderr:err-1\nstdout:out-2\n" {
		return map[string]any{"observed": truncate(replayed.String(), 128)}, errors.New("stdout/stderr replay order or payload differed")
	}
	return map[string]any{"freshCtlCalls": 2, "strictlyIncreasingSequences": true, "orderedPayloads": true, "streams": []string{"stdout", "stderr"}}, nil
}

func (r *runner) checkSpool() (map[string]any, error) {
	script := `import os
chunk=b"x"*16384
for _ in range(640): os.write(1,chunk)`
	started, err := r.start("gate-spool", "/usr/local/bin/python3", "-c", script)
	if err != nil {
		return nil, err
	}
	waited, err := r.wait(started.Command.ID)
	if err != nil {
		return nil, err
	}
	expired, err := r.call(commandruntime.Request{Operation: "connect", CommandID: started.Command.ID, After: 0})
	if err != nil || expired.OK || expired.Code != "CURSOR_EXPIRED" {
		return nil, errors.New("evicted cursor did not return CURSOR_EXPIRED")
	}
	latest := waited.Command.NextSeq - 1
	if _, err = r.connect(started.Command.ID, latest); err != nil {
		return nil, fmt.Errorf("latest cursor was not readable: %w", err)
	}
	logical, allocated, err := directoryUsage(filepath.Join(r.stateDir, "commands", started.Command.ID))
	if err != nil {
		return nil, err
	}
	if logical > commandruntime.MaxSpoolBytes {
		return nil, fmt.Errorf("logical command state %d exceeds %d", logical, commandruntime.MaxSpoolBytes)
	}
	return map[string]any{"cursorExpired": true, "logicalBytes": logical, "allocatedBytes": allocated, "configuredCapBytes": commandruntime.MaxSpoolBytes}, nil
}

func (r *runner) checkStdin() (map[string]any, error) {
	started, err := r.start("gate-stdin", "/usr/local/bin/python3", "-c", "import sys; sys.stdout.buffer.write(sys.stdin.buffer.read())")
	if err != nil {
		return nil, err
	}
	payload := []byte("stdin-round-trip")
	if _, err = r.ok(commandruntime.Request{Operation: "stdin", CommandID: started.Command.ID, Data: payload}); err != nil {
		return nil, err
	}
	if _, err = r.ok(commandruntime.Request{Operation: "closeStdin", CommandID: started.Command.ID}); err != nil {
		return nil, err
	}
	if _, err = r.wait(started.Command.ID); err != nil {
		return nil, err
	}
	output, _, err := r.output(started.Command.ID)
	if err != nil || output != string(payload) {
		return nil, errors.New("stdin round trip mismatch")
	}
	return map[string]any{"write": true, "close": true, "wait": true}, nil
}

func (r *runner) ordinaryDescendant(requestID string) (string, int, error) {
	started, err := r.start(requestID, "/bin/sh", "-c", "sleep 60 & echo $!")
	if err != nil {
		return "", 0, err
	}
	pid, err := r.firstPID(started.Command.ID)
	return started.Command.ID, pid, err
}

func (r *runner) assertGone(pid int) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("ordinary descendant remained after containment signal")
}

func (r *runner) checkSignals() (map[string]any, error) {
	for index, signal := range []string{"TERM", "KILL"} {
		id, pid, err := r.ordinaryDescendant(fmt.Sprintf("gate-signal-%d", index))
		if err != nil {
			return nil, err
		}
		if err = r.signal(id, signal); err != nil {
			return nil, err
		}
		if _, err = r.wait(id); err != nil {
			return nil, err
		}
		if err = r.assertGone(pid); err != nil {
			return nil, fmt.Errorf("%s: %w", signal, err)
		}
	}
	return map[string]any{"leaderExitedBeforeSignal": true, "termDescendantLeak": false, "killDescendantLeak": false}, nil
}

func (r *runner) checkKillAll() (map[string]any, error) {
	var ids []string
	var pids []int
	for index := range 2 {
		id, pid, err := r.ordinaryDescendant(fmt.Sprintf("gate-kill-all-%d", index))
		if err != nil {
			return nil, err
		}
		ids, pids = append(ids, id), append(pids, pid)
	}
	if _, err := r.ok(commandruntime.Request{Operation: "killAll"}); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := r.wait(id); err != nil {
			return nil, err
		}
	}
	for _, pid := range pids {
		if err := r.assertGone(pid); err != nil {
			return nil, err
		}
	}
	return map[string]any{"ordinaryDescendantLeak": false, "commands": 2}, nil
}

func (r *runner) observeSetsidCompatibility() (map[string]any, bool, error) {
	if _, err := os.Stat("/usr/bin/setsid"); err != nil {
		return nil, false, errors.New("setsid utility is unavailable; containment feasibility was not exercised")
	}
	marker := "/workspace/setsid-escape-marker"
	_ = os.Remove(marker)
	script := `/usr/bin/setsid /bin/sh -c 'while :; do printf x >> /workspace/setsid-escape-marker; sleep .05; done' </dev/null >/dev/null 2>&1 & escaped=$!; while [ ! -s /workspace/setsid-escape-marker ]; do sleep .01; done; echo "$escaped"; exec sleep 60`
	started, err := r.start("gate-setsid-escape", "/bin/sh", "-c", script)
	if err != nil {
		return nil, false, err
	}
	escapedPID, err := r.firstPID(started.Command.ID)
	if err != nil {
		return nil, false, err
	}
	r.escaped = append(r.escaped, escapedPID)
	if err = r.signal(started.Command.ID, "KILL"); err != nil {
		return nil, false, err
	}
	if _, err = r.wait(started.Command.ID); err != nil {
		return nil, false, err
	}
	// Measure activity only after the process-group signal has settled, avoiding
	// a final in-flight marker write being mistaken for continued execution.
	time.Sleep(100 * time.Millisecond)
	settled, err := fileSize(marker)
	if err != nil {
		return nil, false, err
	}
	time.Sleep(300 * time.Millisecond)
	after, err := fileSize(marker)
	if err != nil {
		return nil, false, err
	}
	alive := syscall.Kill(escapedPID, 0) == nil
	contained := !alive && after == settled
	cleaned := contained
	if !contained {
		if err = syscall.Kill(escapedPID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return map[string]any{"pgidEscapeContained": false, "escapedPIDAlive": alive, "markerContinuedAfterPGIDKill": after > settled}, false, fmt.Errorf("clean escaped process: %w", err)
		}
		if err = r.assertGone(escapedPID); err != nil {
			return map[string]any{"pgidEscapeContained": false, "escapedPIDAlive": alive, "markerContinuedAfterPGIDKill": after > settled}, false, fmt.Errorf("escaped process cleanup: %w", err)
		}
		cleaned = true
	}
	r.forgetEscaped(escapedPID)
	return map[string]any{
		"pgidEscapeContained":          contained,
		"escapedPIDAlive":              alive,
		"markerContinuedAfterPGIDKill": after > settled,
		"escapedProcessCleanedByGate":  cleaned,
	}, contained, nil
}

func inspectCgroupV2() (map[string]any, error) {
	result := map[string]any{"available": false, "mountType": "none", "writableDelegation": false, "cgroupKill": false}
	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return result, fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	self, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return result, fmt.Errorf("read /proc/self/cgroup: %w", err)
	}
	currentDir, cgroupPath, ok := resolveCurrentCgroupV2(string(mountInfo), string(self))
	result["selfCgroupLines"] = countNonemptyLines(string(self))
	result["unifiedSelfCgroup"] = cgroupPath != ""
	if !ok {
		result["reason"] = "no cgroup v2 directory for this container; runtime may expose only legacy controller mounts"
		return result, nil
	}
	if !pathContains("/sys/fs/cgroup", currentDir) {
		result["reason"] = "the container cgroup v2 directory is not exposed under /sys/fs/cgroup"
		return result, nil
	}
	result["mountType"] = "cgroup2"
	result["currentPath"] = cgroupPath
	controllers, controllersErr := os.ReadFile(filepath.Join(currentDir, "cgroup.controllers"))
	if controllersErr == nil {
		result["controllers"] = strings.Fields(string(controllers))
	}
	subtree, subtreeErr := os.ReadFile(filepath.Join(currentDir, "cgroup.subtree_control"))
	if subtreeErr == nil {
		result["subtreeControl"] = strings.Fields(string(subtree))
	}
	if _, err = os.Stat(filepath.Join(currentDir, "cgroup.kill")); err == nil {
		result["cgroupKill"] = true
	}
	probe := filepath.Join(currentDir, fmt.Sprintf("asp-command-gate-%d", os.Getpid()))
	if err = os.Mkdir(probe, 0o755); err != nil {
		result["reason"] = "current cgroup directory has no writable child subtree: " + errnoName(err)
		return result, nil
	}
	result["writableDelegation"] = true
	if removeErr := os.Remove(probe); removeErr != nil {
		result["available"] = false
		result["reason"] = "writable child cgroup probe could not be removed: " + errnoName(removeErr)
		return result, fmt.Errorf("remove child cgroup probe: %w", removeErr)
	}
	if result["cgroupKill"] == true {
		result["available"] = true
		result["reason"] = "current cgroup exposes a writable child subtree and cgroup.kill"
	} else {
		result["reason"] = "current cgroup exposes a writable child subtree but not cgroup.kill"
	}
	return result, nil
}

func resolveCurrentCgroupV2(mountInfo, selfCgroup string) (string, string, bool) {
	cgroupPath := ""
	for _, line := range strings.Split(strings.TrimSpace(selfCgroup), "\n") {
		if strings.HasPrefix(line, "0::") {
			cgroupPath = filepath.Clean(strings.TrimPrefix(line, "0::"))
			break
		}
	}
	if cgroupPath == "" || !filepath.IsAbs(cgroupPath) {
		return "", cgroupPath, false
	}
	for _, line := range strings.Split(mountInfo, "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 || !strings.HasPrefix(parts[1], "cgroup2 ") {
			continue
		}
		fields := strings.Fields(parts[0])
		if len(fields) < 5 {
			continue
		}
		mountRoot, mountPoint := filepath.Clean(fields[3]), filepath.Clean(fields[4])
		if cgroupPath != mountRoot && !pathContains(mountRoot, cgroupPath) {
			continue
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(cgroupPath, mountRoot), string(filepath.Separator))
		return filepath.Join(mountPoint, relative), cgroupPath, true
	}
	return "", cgroupPath, false
}

func countNonemptyLines(value string) int {
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(value), "\n") {
		if line != "" {
			count++
		}
	}
	return count
}

func pathContains(parent, child string) bool {
	parent, child = filepath.Clean(parent), filepath.Clean(child)
	if parent == string(filepath.Separator) {
		return filepath.IsAbs(child)
	}
	return child == parent || strings.HasPrefix(child, parent+string(filepath.Separator))
}

func directoryUsage(root string) (logical, allocated int64, err error) {
	err = filepath.Walk(root, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode().IsRegular() {
			logical += info.Size()
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				allocated += stat.Blocks * 512
			}
		}
		return nil
	})
	return
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func kernelRelease() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}

func errnoName(err error) string {
	if errors.Is(err, syscall.EROFS) {
		return "read-only filesystem"
	}
	if errors.Is(err, syscall.EPERM) {
		return "operation not permitted"
	}
	if errors.Is(err, syscall.EACCES) {
		return "permission denied"
	}
	return truncate(err.Error(), 128)
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
