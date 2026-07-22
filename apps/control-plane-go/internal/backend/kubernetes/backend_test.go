package kubernetes

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

type fakeResources struct {
	claims           map[string]*unstructured.Unstructured
	deleted          []string
	listError        error
	continueError    error
	listOptions      []metav1.ListOptions
	warmPoolGetError error
	warmPoolReady    int64
}

func newFakeResources() *fakeResources {
	return &fakeResources{claims: make(map[string]*unstructured.Unstructured), warmPoolReady: 1}
}
func (f *fakeResources) Create(_ context.Context, _ schema.GroupVersionResource, _ string, value *unstructured.Unstructured, _ metav1.CreateOptions) (*unstructured.Unstructured, error) {
	copy := value.DeepCopy()
	copy.SetCreationTimestamp(metav1.NewTime(time.Unix(1900000000, 0)))
	_ = unstructured.SetNestedField(copy.Object, copy.GetName(), "status", "sandbox", "name")
	f.claims[copy.GetName()] = copy
	return copy.DeepCopy(), nil
}
func (f *fakeResources) Get(_ context.Context, resource schema.GroupVersionResource, _ string, name string, _ metav1.GetOptions) (*unstructured.Unstructured, error) {
	if resource == warmPoolResource {
		if f.warmPoolGetError != nil {
			return nil, f.warmPoolGetError
		}
		return &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{"replicas": int64(1)}, "status": map[string]any{"readyReplicas": f.warmPoolReady}}}, nil
	}
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
	f.listOptions = append(f.listOptions, options)
	if f.listError != nil {
		return nil, f.listError
	}
	if options.Continue != "" && f.continueError != nil {
		return nil, f.continueError
	}
	names := make([]string, 0, len(f.claims))
	for name, value := range f.claims {
		matches := true
		for _, selector := range strings.Split(options.LabelSelector, ",") {
			parts := strings.SplitN(selector, "=", 2)
			if len(parts) == 2 && value.GetLabels()[parts[0]] != parts[1] {
				matches = false
			}
		}
		if matches {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	start := 0
	if options.Continue != "" {
		var err error
		start, err = strconv.Atoi(strings.TrimPrefix(options.Continue, "offset:"))
		if err != nil || start < 0 || start > len(names) {
			return nil, apierrors.NewResourceExpired("invalid continuation")
		}
	}
	end := len(names)
	if options.Limit > 0 && start+int(options.Limit) < end {
		end = start + int(options.Limit)
	}
	result := &unstructured.UnstructuredList{}
	for _, name := range names[start:end] {
		result.Items = append(result.Items, *f.claims[name].DeepCopy())
	}
	if end < len(names) {
		result.SetContinue(fmt.Sprintf("offset:%d", end))
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

func TestReadyRequiresEveryConfiguredWarmPool(t *testing.T) {
	backend, resources := fixture(t, "gvisor")
	if err := backend.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}

	resources.warmPoolReady = 0
	if err := backend.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "is not fully ready") {
		t.Fatalf("zero-capacity readiness error = %v", err)
	}

	resources.warmPoolReady = 1
	resources.listError = apierrors.NewServiceUnavailable("API unavailable")
	if err := backend.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "list SandboxClaims") {
		t.Fatalf("API readiness error = %v", err)
	}

	resources.listError = nil
	resources.warmPoolGetError = apierrors.NewForbidden(warmPoolResource.GroupResource(), "gvisor-pool", nil)
	if err := backend.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "get WarmPool") {
		t.Fatalf("WarmPool readiness error = %v", err)
	}
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

func TestListUsesExactTenantPoolSelectorAndStableRawPagination(t *testing.T) {
	backend, resources := fixture(t, "gvisor")
	now := time.UnixMilli(1900000000000).UTC()
	putClaim(resources, backend, testScope(), "a-expired", "coding", now.Add(-time.Hour), now)
	putClaim(resources, backend, testScope(), "b-active", "coding", now.Add(-time.Minute), now.Add(time.Minute))
	putClaim(resources, backend, testScope(), "c-deleting", "coding", now.Add(-time.Minute), now.Add(time.Minute))
	deleting := metav1.NewTime(now)
	resources.claims["c-deleting"].SetDeletionTimestamp(&deleting)
	putClaim(resources, backend, lease.Scope{ConsumerID: "consumer-a", SubjectID: "other"}, "d-other-tenant", "coding", now, now.Add(time.Minute))
	putClaim(resources, backend, testScope(), "e-active", "coding", now, now.Add(time.Minute))

	var got []string
	cursor := ""
	emptyPages := 0
	for {
		page, err := backend.List(context.Background(), testScope(), lease.ListRequest{Pool: "coding", Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Leases) == 0 {
			emptyPages++
		}
		for _, record := range page.Leases {
			got = append(got, record.ID)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if strings.Join(got, ",") != "b-active,e-active" || emptyPages != 2 {
		t.Fatalf("listed IDs = %v, empty pages = %d", got, emptyPages)
	}
	expectedSelector := labels.SelectorFromSet(labels.Set{
		managedLabel:  "true",
		scopeLabel:    backend.identity.scopeHash(testScope()),
		poolHashLabel: backend.identity.poolHash("coding"),
	}).String()
	for _, options := range resources.listOptions {
		if options.LabelSelector != expectedSelector || options.Limit != 1 {
			t.Fatalf("list options = %#v, expected selector %q", options, expectedSelector)
		}
	}
}

func TestListCursorFixesActiveAsOfAcrossPages(t *testing.T) {
	resources := newFakeResources()
	clock := time.UnixMilli(1900000000000).UTC()
	pools := map[string]Pool{"coding": {WarmPoolName: "coding-pool", RuntimeClassName: "gvisor", ContainerName: "shell"}}
	backend, err := New(Options{
		Namespace: "platform-test", MetadataSecret: "metadata-secret", Pools: pools,
		Now: func() time.Time { return clock }, ReadyTimeout: 100 * time.Millisecond, PollInterval: time.Millisecond,
		Resources: resources, Pods: fakePods{runtimeClass: "gvisor"}, Runner: fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	putClaim(resources, backend, testScope(), "a-expired", "coding", clock.Add(-time.Minute), clock)
	putClaim(resources, backend, testScope(), "b-was-active", "coding", clock, clock.Add(30*time.Second))
	first, err := backend.List(context.Background(), testScope(), lease.ListRequest{Limit: 1})
	if err != nil || len(first.Leases) != 0 || first.NextCursor == nil {
		t.Fatalf("first page = %#v, error = %v", first, err)
	}
	clock = clock.Add(time.Minute)
	second, err := backend.List(context.Background(), testScope(), lease.ListRequest{Limit: 1, Cursor: *first.NextCursor})
	if err != nil || len(second.Leases) != 1 || second.Leases[0].ID != "b-was-active" {
		t.Fatalf("second page = %#v, error = %v", second, err)
	}
}

func TestListCursorIsBoundAndSurvivesRestart(t *testing.T) {
	resources := newFakeResources()
	pools := map[string]Pool{
		"coding":   {WarmPoolName: "coding-pool", RuntimeClassName: "gvisor", ContainerName: "shell"},
		"research": {WarmPoolName: "research-pool", RuntimeClassName: "gvisor", ContainerName: "shell"},
	}
	backend := newTestBackend(t, resources, pools, "metadata-secret")
	now := time.UnixMilli(1900000000000).UTC()
	putClaim(resources, backend, testScope(), "a", "coding", now, now.Add(time.Minute))
	putClaim(resources, backend, testScope(), "b", "coding", now, now.Add(time.Minute))
	page, err := backend.List(context.Background(), testScope(), lease.ListRequest{Pool: "coding", Limit: 1})
	if err != nil || page.NextCursor == nil {
		t.Fatalf("first page = %#v, error = %v", page, err)
	}
	cursor := *page.NextCursor

	tamperedSuffix := "A"
	if strings.HasSuffix(cursor, tamperedSuffix) {
		tamperedSuffix = "B"
	}
	invalidRequests := []lease.ListRequest{
		{Pool: "coding", Limit: 2, Cursor: cursor},
		{Pool: "research", Limit: 1, Cursor: cursor},
		{Pool: "coding", Limit: 1, Cursor: cursor[:len(cursor)-1] + tamperedSuffix},
	}
	for _, request := range invalidRequests {
		_, err := backend.List(context.Background(), testScope(), request)
		assertLeaseErrorCode(t, err, "INVALID_CURSOR")
	}
	_, err = backend.List(context.Background(), lease.Scope{ConsumerID: "consumer-a", SubjectID: "other"}, lease.ListRequest{Pool: "coding", Limit: 1, Cursor: cursor})
	assertLeaseErrorCode(t, err, "INVALID_CURSOR")

	restarted := newTestBackend(t, resources, pools, "metadata-secret")
	continued, err := restarted.List(context.Background(), testScope(), lease.ListRequest{Pool: "coding", Limit: 1, Cursor: cursor})
	if err != nil || len(continued.Leases) != 1 || continued.Leases[0].ID != "b" {
		t.Fatalf("continued page = %#v, error = %v", continued, err)
	}
}

func TestListMapsExpiredContinuationAndRejectsUnknownPool(t *testing.T) {
	backend, resources := fixture(t, "gvisor")
	now := time.UnixMilli(1900000000000).UTC()
	putClaim(resources, backend, testScope(), "a", "coding", now, now.Add(time.Minute))
	putClaim(resources, backend, testScope(), "b", "coding", now, now.Add(time.Minute))
	page, err := backend.List(context.Background(), testScope(), lease.ListRequest{Limit: 1})
	if err != nil || page.NextCursor == nil {
		t.Fatalf("first page = %#v, error = %v", page, err)
	}
	resources.continueError = apierrors.NewResourceExpired("too old")
	_, err = backend.List(context.Background(), testScope(), lease.ListRequest{Limit: 1, Cursor: *page.NextCursor})
	assertLeaseErrorCode(t, err, "CURSOR_EXPIRED")
	resources.continueError = apierrors.NewGone("continuation is gone")
	_, err = backend.List(context.Background(), testScope(), lease.ListRequest{Limit: 1, Cursor: *page.NextCursor})
	assertLeaseErrorCode(t, err, "CURSOR_EXPIRED")
	_, err = backend.List(context.Background(), testScope(), lease.ListRequest{Pool: "operator-internal-name", Limit: 1})
	assertLeaseErrorCode(t, err, "UNKNOWN_POOL")
	_, _, message := lease.ErrorDetails(err)
	if message != "Unknown sandbox pool" {
		t.Fatalf("unknown Pool leaked details: %q", message)
	}
}

func TestListedLeaseMayBeReleasedBeforeConnect(t *testing.T) {
	backend, resources := fixture(t, "gvisor")
	now := time.UnixMilli(1900000000000).UTC()
	putClaim(resources, backend, testScope(), "race", "coding", now, now.Add(time.Minute))
	page, err := backend.List(context.Background(), testScope(), lease.ListRequest{Limit: 50})
	if err != nil || len(page.Leases) != 1 {
		t.Fatalf("page = %#v, error = %v", page, err)
	}
	if _, err := backend.Release(context.Background(), testScope(), "race"); err != nil {
		t.Fatal(err)
	}
	_, err = backend.Get(context.Background(), testScope(), page.Leases[0].ID)
	assertLeaseErrorCode(t, err, "LEASE_NOT_FOUND")
}

func newTestBackend(t *testing.T, resources *fakeResources, pools map[string]Pool, secret string) *Backend {
	t.Helper()
	backend, err := New(Options{
		Namespace: "platform-test", MetadataSecret: secret, Pools: pools,
		Now:          func() time.Time { return time.UnixMilli(1900000000000) },
		ReadyTimeout: 100 * time.Millisecond, PollInterval: time.Millisecond,
		Resources: resources, Pods: fakePods{runtimeClass: "gvisor"}, Runner: fakeRunner{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func putClaim(resources *fakeResources, backend *Backend, scope lease.Scope, id, pool string, created, expires time.Time) {
	record := lease.Record{ID: id, Pool: pool, Status: lease.StatusActive, CreatedAt: created, ExpiresAt: expires, LastUsedAt: created}
	resources.claims[id] = buildClaim(record, scope, id+"-key", backend.pools[pool], backend.identity)
}

func assertLeaseErrorCode(t *testing.T, err error, expected string) {
	t.Helper()
	_, code, _ := lease.ErrorDetails(err)
	if code != expected {
		t.Fatalf("error = %v, code = %q, expected %q", err, code, expected)
	}
}
