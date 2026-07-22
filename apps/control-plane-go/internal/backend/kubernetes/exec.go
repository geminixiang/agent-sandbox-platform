package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	posixpath "path"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	clientremotecommand "k8s.io/client-go/tools/remotecommand"
	utilsexec "k8s.io/client-go/util/exec"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

const maxCommandOutputBytes = 10 * 1024 * 1024

type podTarget struct{ pod, container string }
type commandRunner interface {
	Run(context.Context, string, podTarget, []string, io.Reader, io.Writer, io.Writer) error
}

type spdyCommandRunner struct {
	config *rest.Config
	client coreclient.CoreV1Interface
}

func (r spdyCommandRunner) Run(ctx context.Context, namespace string, target podTarget, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	request := r.client.RESTClient().Post().Resource("pods").Name(target.pod).Namespace(namespace).SubResource("exec").VersionedParams(&corev1.PodExecOptions{Container: target.container, Command: command, Stdin: stdin != nil, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	executor, err := clientremotecommand.NewSPDYExecutor(r.config, "POST", request.URL())
	if err != nil {
		return err
	}
	return executor.StreamWithContext(ctx, clientremotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr})
}

func (b *Backend) requireActiveTarget(ctx context.Context, scope lease.Scope, id string) (podTarget, error) {
	claim, err := b.requireClaim(ctx, scope, id)
	if err != nil {
		return podTarget{}, err
	}
	record, err := recordFromClaim(claim)
	if err != nil {
		return podTarget{}, err
	}
	if !record.ExpiresAt.After(b.now()) {
		_ = b.stopLeaseAndDelete(context.WithoutCancel(ctx), id)
		return podTarget{}, lease.NewError(409, "LEASE_NOT_ACTIVE", "Lease is not active")
	}
	return b.targetForClaim(ctx, claim, record.Pool)
}

func (b *Backend) targetForClaim(ctx context.Context, claim *unstructured.Unstructured, poolName string) (podTarget, error) {
	pool, ok := b.pools[poolName]
	if !ok {
		return podTarget{}, lease.NewError(500, "POOL_CONFIGURATION_MISSING", "Lease pool configuration is unavailable")
	}
	sandboxName, found, err := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
	if err != nil || !found || sandboxName == "" {
		return podTarget{}, lease.NewError(503, "SANDBOX_NOT_READY", "Sandbox is not ready")
	}
	sandbox, err := b.resources.Get(ctx, sandboxResource, b.namespace, sandboxName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return podTarget{}, lease.NewError(503, "SANDBOX_NOT_READY", "Sandbox is not ready")
		}
		return podTarget{}, err
	}
	podName := sandbox.GetAnnotations()["agents.x-k8s.io/pod-name"]
	if podName == "" {
		podName = sandboxName
	}
	pod, err := b.pods.Get(ctx, b.namespace, podName, metav1.GetOptions{})
	if err != nil {
		return podTarget{}, err
	}
	container := pool.ContainerName
	if container == "" && len(pod.Spec.Containers) > 0 {
		container = pod.Spec.Containers[0].Name
	}
	if container == "" {
		return podTarget{}, lease.NewError(502, "SANDBOX_CONTAINER_NOT_FOUND", "Sandbox Pod has no container")
	}
	return podTarget{pod: podName, container: container}, nil
}

func (b *Backend) runCommand(ctx context.Context, target podTarget, command []string, stdin string, timeoutSeconds *int) (lease.ExecResult, error) {
	if timeoutSeconds != nil {
		if *timeoutSeconds <= 0 {
			return lease.ExecResult{}, lease.NewError(400, "INVALID_REQUEST", "'timeoutSeconds' must be a positive integer")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*timeoutSeconds)*time.Second)
		defer cancel()
	}
	stdout, stderr := &limitedBuffer{}, &limitedBuffer{}
	var input io.Reader
	if stdin != "" {
		input = strings.NewReader(stdin)
	}
	err := b.runner.Run(ctx, b.namespace, target, command, input, stdout, stderr)
	if stdout.exceeded || stderr.exceeded {
		return lease.ExecResult{}, lease.NewError(413, "OUTPUT_TOO_LARGE", "Command output exceeded 10 MiB")
	}
	if err == nil {
		return lease.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}, nil
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return lease.ExecResult{}, lease.NewError(408, "ABORTED", "Command was aborted or timed out")
	}
	var exitErr utilsexec.ExitError
	if errors.As(err, &exitErr) {
		return lease.ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), Code: exitErr.ExitStatus()}, nil
	}
	return lease.ExecResult{}, fmt.Errorf("stream Pod exec: %w", err)
}

func normalizeWorkspacePath(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		value = lease.WorkspacePath
	}
	if !strings.HasPrefix(value, "/") {
		value = posixpath.Join(lease.WorkspacePath, value)
	}
	value = posixpath.Clean(value)
	if value != lease.WorkspacePath && !strings.HasPrefix(value, lease.WorkspacePath+"/") {
		return "", lease.NewError(400, "INVALID_PATH", "Path must stay inside /workspace")
	}
	return value, nil
}

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func shellCommand(cwd string, env map[string]string, command string) (string, error) {
	keys := make([]string, 0, len(env))
	for key := range env {
		if !environmentName.MatchString(key) {
			return "", lease.NewError(400, "INVALID_REQUEST", "environment variable names must be valid shell identifiers")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := []string{"cd " + shellQuote(cwd)}
	for _, key := range keys {
		parts = append(parts, "export "+key+"="+shellQuote(env[key]))
	}
	parts = append(parts, command)
	return strings.Join(parts, " && "), nil
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

type limitedBuffer struct {
	buffer   bytes.Buffer
	exceeded bool
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	length := len(value)
	remaining := maxCommandOutputBytes - b.buffer.Len()
	if remaining > 0 {
		_, _ = b.buffer.Write(value[:min(remaining, length)])
	}
	if length > remaining {
		b.exceeded = true
	}
	return length, nil
}

func (b *limitedBuffer) String() string { return b.buffer.String() }
