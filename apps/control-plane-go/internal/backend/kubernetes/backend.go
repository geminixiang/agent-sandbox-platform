package kubernetes

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	posixpath "path"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

const (
	managedLabel      = "sandbox.geminixiang.dev/managed"
	scopeLabel        = "sandbox.geminixiang.dev/scope"
	consumerLabel     = "sandbox.geminixiang.dev/consumer"
	idempotencyLabel  = "sandbox.geminixiang.dev/idempotency"
	poolHashLabel     = "sandbox.geminixiang.dev/pool"
	poolAnnotation    = "sandbox.geminixiang.dev/pool-name"
	createdAnnotation = "sandbox.geminixiang.dev/created-at"
	expiresAnnotation = "sandbox.geminixiang.dev/expires-at"
)

type Pool struct {
	WarmPoolName     string `json:"warmPoolName"`
	RuntimeClassName string `json:"runtimeClassName"`
	ContainerName    string `json:"containerName,omitempty"`
}

type Options struct {
	Namespace         string
	MetadataSecret    string
	Pools             map[string]Pool
	DefaultTTLSeconds int
	MaxTTLSeconds     int
	ReadyTimeout      time.Duration
	PollInterval      time.Duration
	Now               func() time.Time
	Resources         resourceClient
	Pods              podClient
	Runner            commandRunner
}

type Backend struct {
	namespace                  string
	identity                   metadataIdentity
	pools                      map[string]Pool
	defaultTTL, maxTTL         int
	readyTimeout, pollInterval time.Duration
	now                        func() time.Time
	resources                  resourceClient
	pods                       podClient
	runner                     commandRunner
	acquireMu                  sync.Mutex
}

func New(options Options) (*Backend, error) {
	if strings.TrimSpace(options.Namespace) == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	identity, err := newMetadataIdentity(options.MetadataSecret)
	if err != nil {
		return nil, err
	}
	pools, err := validatePools(options.Pools)
	if err != nil {
		return nil, err
	}
	if options.DefaultTTLSeconds == 0 {
		options.DefaultTTLSeconds = 900
	}
	if options.MaxTTLSeconds == 0 {
		options.MaxTTLSeconds = 3600
	}
	if options.ReadyTimeout == 0 {
		options.ReadyTimeout = 2 * time.Minute
	}
	if options.PollInterval == 0 {
		options.PollInterval = 500 * time.Millisecond
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Resources == nil || options.Pods == nil || options.Runner == nil {
		return nil, fmt.Errorf("Kubernetes clients and command runner are required")
	}
	return &Backend{namespace: options.Namespace, identity: identity, pools: pools, defaultTTL: options.DefaultTTLSeconds, maxTTL: options.MaxTTLSeconds, readyTimeout: options.ReadyTimeout, pollInterval: options.PollInterval, now: options.Now, resources: options.Resources, pods: options.Pods, runner: options.Runner}, nil
}

func NewFromConfig(config *rest.Config, options Options) (*Backend, error) {
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes dynamic client: %w", err)
	}
	coreClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes core client: %w", err)
	}
	options.Resources = dynamicResources{client: dynamicClient}
	options.Pods = corePods{client: coreClient.CoreV1()}
	options.Runner = spdyCommandRunner{config: config, client: coreClient.CoreV1()}
	return New(options)
}

func LoadConfig(kubeconfig, contextName string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

func (b *Backend) Acquire(ctx context.Context, scope lease.Scope, request lease.AcquireRequest) (lease.AcquireResult, error) {
	b.acquireMu.Lock()
	defer b.acquireMu.Unlock()
	pool, ok := b.pools[request.Pool]
	if !ok {
		return lease.AcquireResult{}, lease.NewError(400, "UNKNOWN_POOL", "Unknown sandbox pool")
	}
	ttl := b.defaultTTL
	if request.TTLSeconds != nil {
		ttl = *request.TTLSeconds
	}
	if ttl <= 0 || ttl > b.maxTTL {
		return lease.AcquireResult{}, lease.NewError(400, "INVALID_LEASE_TTL", fmt.Sprintf("ttlSeconds must be an integer between 1 and %d", b.maxTTL))
	}
	if existing, found, err := b.findByIdempotencyKey(ctx, scope, request.IdempotencyKey); err != nil {
		return lease.AcquireResult{}, err
	} else if found {
		return lease.AcquireResult{Lease: existing, Replayed: true}, nil
	}
	now := b.now().UTC()
	record := lease.Record{ID: newLeaseID(), Pool: request.Pool, Status: lease.StatusActive, CreatedAt: now, ExpiresAt: now.Add(time.Duration(ttl) * time.Second), LastUsedAt: now}
	claim := buildClaim(record, scope, request.IdempotencyKey, pool, b.identity)
	if _, err := b.resources.Create(ctx, claimResource, b.namespace, claim, metav1.CreateOptions{}); err != nil {
		return lease.AcquireResult{}, fmt.Errorf("create SandboxClaim: %w", err)
	}
	ready, err := b.waitForReadyClaim(ctx, record.ID)
	if err != nil {
		_ = b.deleteClaim(context.WithoutCancel(ctx), record.ID)
		return lease.AcquireResult{}, err
	}
	if err := b.verifyRuntime(ctx, ready, pool); err != nil {
		_ = b.deleteClaim(context.WithoutCancel(ctx), record.ID)
		return lease.AcquireResult{}, err
	}
	return lease.AcquireResult{Lease: record}, nil
}

func (b *Backend) Get(ctx context.Context, scope lease.Scope, id string) (lease.Record, error) {
	claim, err := b.requireClaim(ctx, scope, id)
	if err != nil {
		return lease.Record{}, err
	}
	record, err := recordFromClaim(claim)
	if err != nil {
		return lease.Record{}, err
	}
	if !b.now().Before(record.ExpiresAt) {
		_ = b.deleteClaim(context.WithoutCancel(ctx), id)
		record.Status = lease.StatusExpired
	}
	return record, nil
}

func (b *Backend) Release(ctx context.Context, scope lease.Scope, id string) (lease.Record, error) {
	claim, err := b.requireClaim(ctx, scope, id)
	if err != nil {
		return lease.Record{}, err
	}
	record, err := recordFromClaim(claim)
	if err != nil {
		return lease.Record{}, err
	}
	if !b.now().Before(record.ExpiresAt) {
		record.Status = lease.StatusExpired
	} else {
		record.Status = lease.StatusReleased
	}
	if err := b.deleteClaim(ctx, id); err != nil {
		return lease.Record{}, err
	}
	return record, nil
}
func (b *Backend) Delete(ctx context.Context, scope lease.Scope, id string) error {
	if _, err := b.requireClaim(ctx, scope, id); err != nil {
		return err
	}
	return b.deleteClaim(ctx, id)
}
func (b *Backend) Ready(ctx context.Context) error {
	if _, err := b.resources.List(ctx, claimResource, b.namespace, metav1.ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("list SandboxClaims: %w", err)
	}
	for name, pool := range b.pools {
		warmPool, err := b.resources.Get(ctx, warmPoolResource, b.namespace, pool.WarmPoolName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get WarmPool for Pool %q: %w", name, err)
		}
		ready, found, err := unstructured.NestedInt64(warmPool.Object, "status", "readyReplicas")
		if err != nil || !found || ready < 1 {
			return fmt.Errorf("WarmPool %q for Pool %q has no ready replicas", pool.WarmPoolName, name)
		}
	}
	return nil
}

func (b *Backend) Close(context.Context) error { return nil }

func (b *Backend) Exec(ctx context.Context, scope lease.Scope, id string, request lease.ExecRequest) (lease.ExecResult, error) {
	target, err := b.requireActiveTarget(ctx, scope, id)
	if err != nil {
		return lease.ExecResult{}, err
	}
	cwd, err := normalizeWorkspacePath(request.CWD)
	if err != nil {
		return lease.ExecResult{}, err
	}
	command, err := shellCommand(cwd, request.Env, request.Command)
	if err != nil {
		return lease.ExecResult{}, err
	}
	return b.runCommand(ctx, target, []string{"sh", "-lc", command}, "", request.TimeoutSeconds)
}

func (b *Backend) ReadFile(ctx context.Context, scope lease.Scope, id string, request lease.ReadFileRequest) (lease.ReadFileResult, error) {
	target, err := b.requireActiveTarget(ctx, scope, id)
	if err != nil {
		return lease.ReadFileResult{}, err
	}
	path, err := normalizeWorkspacePath(request.Path)
	if err != nil {
		return lease.ReadFileResult{}, err
	}
	encoding := request.Encoding
	if encoding == "" {
		encoding = "utf8"
	}
	command := "cat -- " + shellQuote(path)
	if encoding == "base64" {
		command = "base64 < " + shellQuote(path) + " | tr -d '\\n'"
	}
	result, err := b.runCommand(ctx, target, []string{"sh", "-lc", command}, "", nil)
	if err != nil {
		return lease.ReadFileResult{}, err
	}
	if result.Code != 0 {
		return lease.ReadFileResult{}, lease.NewError(502, "FILE_READ_FAILED", "Workspace file could not be read")
	}
	return lease.ReadFileResult{Path: request.Path, Content: result.Stdout, Encoding: encoding}, nil
}

func (b *Backend) WriteFile(ctx context.Context, scope lease.Scope, id string, request lease.WriteFileRequest) error {
	target, err := b.requireActiveTarget(ctx, scope, id)
	if err != nil {
		return err
	}
	path, err := normalizeWorkspacePath(request.Path)
	if err != nil {
		return err
	}
	content := request.Content
	if request.Encoding != "base64" {
		content = base64.StdEncoding.EncodeToString([]byte(content))
	}
	command := "mkdir -p -- " + shellQuote(posixpath.Dir(path)) + " && base64 -d > " + shellQuote(path)
	result, err := b.runCommand(ctx, target, []string{"sh", "-lc", command}, content, nil)
	if err != nil {
		return err
	}
	if result.Code != 0 {
		return lease.NewError(502, "FILE_WRITE_FAILED", "Workspace file could not be written")
	}
	return nil
}

func (b *Backend) FindByIdempotencyKey(ctx context.Context, scope lease.Scope, key string) (lease.Record, bool, error) {
	return b.findByIdempotencyKey(ctx, scope, key)
}

type ActiveLease struct {
	Record       lease.Record
	ScopeHash    string
	ConsumerHash string
}

func (b *Backend) ListActiveLeases(ctx context.Context) ([]ActiveLease, error) {
	list, err := b.resources.List(ctx, claimResource, b.namespace, metav1.ListOptions{LabelSelector: labels.Set{managedLabel: "true"}.String()})
	if err != nil {
		return nil, err
	}
	active := make([]ActiveLease, 0, len(list.Items))
	for index := range list.Items {
		claim := &list.Items[index]
		if claim.GetDeletionTimestamp() != nil {
			continue
		}
		record, err := recordFromClaim(claim)
		if err != nil {
			return nil, err
		}
		if record.ExpiresAt.After(b.now()) {
			active = append(active, ActiveLease{Record: record, ScopeHash: claim.GetLabels()[scopeLabel], ConsumerHash: claim.GetLabels()[consumerLabel]})
		}
	}
	return active, nil
}

func (b *Backend) Recover(ctx context.Context) ([]ActiveLease, error) {
	return b.ListActiveLeases(ctx)
}

func (b *Backend) SweepExpired(ctx context.Context) error {
	list, err := b.resources.List(ctx, claimResource, b.namespace, metav1.ListOptions{LabelSelector: labels.Set{managedLabel: "true"}.String()})
	if err != nil {
		return err
	}
	for index := range list.Items {
		claim := &list.Items[index]
		if claim.GetDeletionTimestamp() != nil {
			continue
		}
		record, err := recordFromClaim(claim)
		if err != nil {
			return err
		}
		if !record.ExpiresAt.After(b.now()) {
			if err := b.deleteClaim(ctx, record.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Backend) findByIdempotencyKey(ctx context.Context, scope lease.Scope, key string) (lease.Record, bool, error) {
	selector := labels.Set{managedLabel: "true", scopeLabel: b.identity.scopeHash(scope), idempotencyLabel: b.identity.idempotencyHash(scope, key)}.String()
	list, err := b.resources.List(ctx, claimResource, b.namespace, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return lease.Record{}, false, err
	}
	for index := range list.Items {
		claim := &list.Items[index]
		if claim.GetDeletionTimestamp() != nil {
			continue
		}
		record, err := recordFromClaim(claim)
		if err != nil {
			return lease.Record{}, false, err
		}
		if !b.now().Before(record.ExpiresAt) {
			_ = b.deleteClaim(context.WithoutCancel(ctx), record.ID)
			continue
		}
		return record, true, nil
	}
	return lease.Record{}, false, nil
}

func (b *Backend) requireClaim(ctx context.Context, scope lease.Scope, id string) (*unstructured.Unstructured, error) {
	claim, err := b.resources.Get(ctx, claimResource, b.namespace, id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, lease.NotFound()
	}
	if err != nil {
		return nil, err
	}
	if claim.GetDeletionTimestamp() != nil || claim.GetLabels()[managedLabel] != "true" || claim.GetLabels()[scopeLabel] != b.identity.scopeHash(scope) {
		return nil, lease.NotFound()
	}
	return claim, nil
}

func (b *Backend) waitForReadyClaim(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	ctx, cancel := context.WithTimeout(ctx, b.readyTimeout)
	defer cancel()
	for {
		claim, err := b.resources.Get(ctx, claimResource, b.namespace, name, metav1.GetOptions{})
		if err == nil {
			sandboxName, _, _ := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
			if sandboxName != "" {
				return claim, nil
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, lease.NewError(504, "SANDBOX_READY_TIMEOUT", "Sandbox did not become ready")
		case <-time.After(b.pollInterval):
		}
	}
}

func (b *Backend) verifyRuntime(ctx context.Context, claim *unstructured.Unstructured, pool Pool) error {
	sandboxName, _, _ := unstructured.NestedString(claim.Object, "status", "sandbox", "name")
	sandbox, err := b.resources.Get(ctx, sandboxResource, b.namespace, sandboxName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	podName := sandbox.GetAnnotations()["agents.x-k8s.io/pod-name"]
	if podName == "" {
		return lease.NewError(502, "SANDBOX_POD_NOT_FOUND", "Sandbox Pod was not reported")
	}
	pod, err := b.pods.Get(ctx, b.namespace, podName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != pool.RuntimeClassName {
		return lease.NewError(502, "RUNTIME_CLASS_MISMATCH", "Sandbox runtime class does not match Pool policy")
	}
	container := pool.ContainerName
	if container == "" && len(pod.Spec.Containers) > 0 {
		container = pod.Spec.Containers[0].Name
	}
	if container == "" {
		return lease.NewError(502, "SANDBOX_CONTAINER_NOT_FOUND", "Sandbox Pod has no container")
	}
	return nil
}

func (b *Backend) deleteClaim(ctx context.Context, name string) error {
	policy := metav1.DeletePropagationForeground
	err := b.resources.Delete(ctx, claimResource, b.namespace, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func validatePools(input map[string]Pool) (map[string]Pool, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("at least one pool is required")
	}
	result := make(map[string]Pool, len(input))
	for name, pool := range input {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(pool.WarmPoolName) == "" || strings.TrimSpace(pool.RuntimeClassName) == "" {
			return nil, fmt.Errorf("pool %q requires warmPoolName and runtimeClassName", name)
		}
		result[name] = pool
	}
	return result, nil
}

func buildClaim(record lease.Record, scope lease.Scope, key string, pool Pool, identity metadataIdentity) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxClaim", "metadata": map[string]any{"name": record.ID, "labels": map[string]any{managedLabel: "true", scopeLabel: identity.scopeHash(scope), consumerLabel: identity.consumerHash(scope), idempotencyLabel: identity.idempotencyHash(scope, key), poolHashLabel: identity.poolHash(record.Pool)}, "annotations": map[string]any{poolAnnotation: record.Pool, createdAnnotation: record.CreatedAt.Format(time.RFC3339Nano), expiresAnnotation: record.ExpiresAt.Format(time.RFC3339Nano)}}, "spec": map[string]any{"warmPoolRef": map[string]any{"name": pool.WarmPoolName}, "lifecycle": map[string]any{"shutdownPolicy": "DeleteForeground", "shutdownTime": record.ExpiresAt.Format(time.RFC3339Nano)}}}}
}
func recordFromClaim(claim *unstructured.Unstructured) (lease.Record, error) {
	a := claim.GetAnnotations()
	created, err := time.Parse(time.RFC3339Nano, a[createdAnnotation])
	if err != nil {
		return lease.Record{}, fmt.Errorf("invalid claim created-at: %w", err)
	}
	expires, err := time.Parse(time.RFC3339Nano, a[expiresAnnotation])
	if err != nil {
		return lease.Record{}, fmt.Errorf("invalid claim expires-at: %w", err)
	}
	return lease.Record{ID: claim.GetName(), Pool: a[poolAnnotation], Status: lease.StatusActive, CreatedAt: created, ExpiresAt: expires, LastUsedAt: created}, nil
}
func newLeaseID() string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return "lease-" + hex.EncodeToString(value)
}
