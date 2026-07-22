package kubernetes

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

type fakeResources struct {
	claims  map[string]*unstructured.Unstructured
	deleted []string
}

func newFakeResources() *fakeResources {
	return &fakeResources{claims: make(map[string]*unstructured.Unstructured)}
}
func (f *fakeResources) Create(_ context.Context, _ schema.GroupVersionResource, _ string, value *unstructured.Unstructured, _ metav1.CreateOptions) (*unstructured.Unstructured, error) {
	copy := value.DeepCopy()
	copy.SetCreationTimestamp(metav1.NewTime(time.Unix(1900000000, 0)))
	_ = unstructured.SetNestedField(copy.Object, copy.GetName(), "status", "sandbox", "name")
	f.claims[copy.GetName()] = copy
	return copy.DeepCopy(), nil
}
func (f *fakeResources) Get(_ context.Context, resource schema.GroupVersionResource, _ string, name string, _ metav1.GetOptions) (*unstructured.Unstructured, error) {
	if resource == sandboxResource {
		return &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name, "annotations": map[string]any{"agents.x-k8s.io/pod-name": name}}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}}, nil
	}
	value := f.claims[name]
	if value == nil {
		return nil, apierrors.NewNotFound(resource.GroupResource(), name)
	}
	return value.DeepCopy(), nil
}
func (f *fakeResources) List(_ context.Context, _ schema.GroupVersionResource, _ string, options metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	result := &unstructured.UnstructuredList{}
	for _, value := range f.claims {
		matches := true
		for _, selector := range strings.Split(options.LabelSelector, ",") {
			parts := strings.SplitN(selector, "=", 2)
			if len(parts) == 2 && value.GetLabels()[parts[0]] != parts[1] {
				matches = false
			}
		}
		if matches {
			result.Items = append(result.Items, *value.DeepCopy())
		}
	}
	return result, nil
}
func (f *fakeResources) Delete(_ context.Context, _ schema.GroupVersionResource, _ string, name string, _ metav1.DeleteOptions) error {
	f.deleted = append(f.deleted, name)
	delete(f.claims, name)
	return nil
}

type fakePods struct{ runtimeClass string }
type fakeRunner struct{}

func (fakeRunner) Run(context.Context, string, podTarget, []string, io.Reader, io.Writer, io.Writer) error {
	return nil
}

func (f fakePods) Get(_ context.Context, namespace, name string, _ metav1.GetOptions) (*corev1.Pod, error) {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: corev1.PodSpec{RuntimeClassName: &f.runtimeClass, Containers: []corev1.Container{{Name: "shell"}}}}, nil
}

func fixture(t *testing.T, runtimeClass string) (*Backend, *fakeResources) {
	t.Helper()
	resources := newFakeResources()
	backend, err := New(Options{Namespace: "platform-test", MetadataSecret: "metadata-secret", Pools: map[string]Pool{"coding": {WarmPoolName: "gvisor-pool", RuntimeClassName: "gvisor", ContainerName: "shell"}}, Now: func() time.Time { return time.UnixMilli(1900000000000) }, ReadyTimeout: 100 * time.Millisecond, PollInterval: time.Millisecond, Resources: resources, Pods: fakePods{runtimeClass: runtimeClass}, Runner: fakeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	return backend, resources
}

func TestAcquireUsesHashedMetadataAndServerSidePoolMapping(t *testing.T) {
	backend, resources := fixture(t, "gvisor")
	scope := testScope()
	result, err := backend.Acquire(context.Background(), scope, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	claim := resources.claims[result.Lease.ID]
	warmPool, _, _ := unstructured.NestedString(claim.Object, "spec", "warmPoolRef", "name")
	if warmPool != "gvisor-pool" {
		t.Fatalf("warm pool = %q", warmPool)
	}
	metadata := claim.GetLabels()[scopeLabel] + claim.GetLabels()[consumerLabel] + claim.GetLabels()[idempotencyLabel]
	for _, secret := range []string{scope.ConsumerID, scope.SubjectID, "request-1"} {
		if strings.Contains(metadata, secret) {
			t.Fatalf("metadata disclosed %q", secret)
		}
	}
	if len(claim.GetLabels()[scopeLabel]) != 40 {
		t.Fatalf("scope hash length = %d", len(claim.GetLabels()[scopeLabel]))
	}
}
func TestAcquireReplaysOnlyWithinTenantScope(t *testing.T) {
	backend, _ := fixture(t, "gvisor")
	first, err := backend.Acquire(context.Background(), testScope(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	replay, err := backend.Acquire(context.Background(), testScope(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Replayed || replay.Lease.ID != first.Lease.ID {
		t.Fatalf("unexpected replay: %#v", replay)
	}
	other := testScope()
	other.SubjectID = "subject-b"
	second, err := backend.Acquire(context.Background(), other, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if second.Lease.ID == first.Lease.ID {
		t.Fatal("cross-scope idempotency collision")
	}
}
func TestCrossScopeAndMissingClaimsAreIndistinguishable(t *testing.T) {
	backend, _ := fixture(t, "gvisor")
	result, err := backend.Acquire(context.Background(), testScope(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	other := testScope()
	other.SubjectID = "subject-b"
	for _, id := range []string{result.Lease.ID, "lease-missing"} {
		_, err = backend.Get(context.Background(), other, id)
		status, code, message := lease.ErrorDetails(err)
		if status != 404 || code != "LEASE_NOT_FOUND" || message != "Lease not found" {
			t.Fatalf("disclosure for %s: %v", id, err)
		}
	}
}
func TestRuntimeMismatchDeletesClaim(t *testing.T) {
	backend, resources := fixture(t, "runc")
	_, err := backend.Acquire(context.Background(), testScope(), testRequest())
	_, code, _ := lease.ErrorDetails(err)
	if code != "RUNTIME_CLASS_MISMATCH" {
		t.Fatalf("error = %v", err)
	}
	if len(resources.claims) != 0 || len(resources.deleted) != 1 {
		t.Fatalf("claim was not cleaned up")
	}
}

func testScope() lease.Scope { return lease.Scope{ConsumerID: "mikan", SubjectID: "subject-a"} }
func testRequest() lease.AcquireRequest {
	ttl := 60
	return lease.AcquireRequest{Pool: "coding", TTLSeconds: &ttl, IdempotencyKey: "request-1"}
}

type scriptedRunner struct {
	stdout string
	stderr string
	err    error
	stdin  string
}

func (r *scriptedRunner) Run(_ context.Context, _ string, _ podTarget, _ []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if stdin != nil {
		value, _ := io.ReadAll(stdin)
		r.stdin = string(value)
	}
	_, _ = io.WriteString(stdout, r.stdout)
	_, _ = io.WriteString(stderr, r.stderr)
	return r.err
}

func TestWorkspacePathsStayConfined(t *testing.T) {
	for _, value := range []string{"../secret", "/etc/passwd", "/workspace/../../etc/passwd"} {
		if _, err := normalizeWorkspacePath(value); err == nil {
			t.Fatalf("accepted escaping path %q", value)
		}
	}
	for input, expected := range map[string]string{"": "/workspace", "file.txt": "/workspace/file.txt", "/workspace/a/../b": "/workspace/b"} {
		actual, err := normalizeWorkspacePath(input)
		if err != nil || actual != expected {
			t.Fatalf("normalizeWorkspacePath(%q) = %q, %v", input, actual, err)
		}
	}
}

func TestCommandOutputIsBounded(t *testing.T) {
	runner := &scriptedRunner{stdout: strings.Repeat("a", maxCommandOutputBytes+1)}
	backend := &Backend{namespace: "test", runner: runner}
	_, err := backend.runCommand(context.Background(), podTarget{}, []string{"true"}, "", nil)
	_, code, _ := lease.ErrorDetails(err)
	if code != "OUTPUT_TOO_LARGE" {
		t.Fatalf("error = %v", err)
	}
}

func TestShellEnvironmentRejectsInjection(t *testing.T) {
	if _, err := shellCommand("/workspace", map[string]string{"SAFE;touch /tmp/pwned": "x"}, "true"); err == nil {
		t.Fatal("accepted invalid environment name")
	}
	command, err := shellCommand("/workspace", map[string]string{"SAFE": "a'b"}, "true")
	if err != nil || !strings.Contains(command, "export SAFE='a'\\''b'") {
		t.Fatalf("command = %q, error = %v", command, err)
	}
}

func TestRecoveryAndExpirySweepIgnoreDeletingClaims(t *testing.T) {
	backend, resources := fixture(t, "gvisor")
	active, err := backend.Acquire(context.Background(), testScope(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	expiredRequest := testRequest()
	expiredRequest.IdempotencyKey = "expired"
	expired, err := backend.Acquire(context.Background(), testScope(), expiredRequest)
	if err != nil {
		t.Fatal(err)
	}
	annotations := resources.claims[expired.Lease.ID].GetAnnotations()
	annotations[expiresAnnotation] = time.UnixMilli(1899999999000).UTC().Format(time.RFC3339Nano)
	resources.claims[expired.Lease.ID].SetAnnotations(annotations)
	deleting := resources.claims[active.Lease.ID].DeepCopy()
	now := metav1.NewTime(time.UnixMilli(1900000000000))
	deleting.SetDeletionTimestamp(&now)
	resources.claims["deleting"] = deleting

	recovered, err := backend.Recover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].Record.ID != active.Lease.ID {
		t.Fatalf("recovered = %#v", recovered)
	}
	if err := backend.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resources.claims[expired.Lease.ID] != nil {
		t.Fatal("expired claim was not deleted")
	}
}
