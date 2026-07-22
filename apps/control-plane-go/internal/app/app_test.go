package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

	kubernetesbackend "github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/backend/kubernetes"
	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

func TestLoadConfigRequiresKubernetesState(t *testing.T) {
	values := map[string]string{
		"SANDBOX_K8S_NAMESPACE":                "platform",
		"SANDBOX_METADATA_SECRET":              "metadata",
		"SANDBOX_CONSUMER_SECRETS":             `{"mikan":"secret"}`,
		"SANDBOX_K8S_POOLS":                    `{"coding":{"warmPoolName":"pool","runtimeClassName":"gvisor","containerName":"shell"}}`,
		"SANDBOX_SWEEP_INTERVAL":               "5s",
		"SANDBOX_FILE_TRANSFER_MAX_CONCURRENT": "6",
		"SANDBOX_FILE_TRANSFER_MAX_PER_LEASE":  "2",
		"SANDBOX_FILE_TRANSFER_TIMEOUT":        "45s",
	}
	config, err := LoadConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if config.Namespace != "platform" || config.SweepInterval != 5*time.Second || config.FileTransferMaxConcurrent != 6 || config.FileTransferMaxPerLease != 2 || config.FileTransferTimeout != 45*time.Second || config.Pools["coding"].WarmPoolName != "pool" {
		t.Fatalf("config = %#v", config)
	}
	values["SANDBOX_FILE_TRANSFER_MAX_PER_LEASE"] = "7"
	if _, err := LoadConfig(func(name string) string { return values[name] }); err == nil {
		t.Fatal("accepted per-Lease transfer limit above global limit")
	}
	values["SANDBOX_FILE_TRANSFER_MAX_PER_LEASE"] = "2"
	delete(values, "SANDBOX_METADATA_SECRET")
	if _, err := LoadConfig(func(name string) string { return values[name] }); err == nil {
		t.Fatal("accepted missing metadata secret")
	}
}

func TestAppRecoversAndSweepsBeforeServing(t *testing.T) {
	backend := &fakeLifecycleBackend{}
	config := Config{Address: "127.0.0.1:0", ConsumerSecrets: map[string]string{"mikan": "secret"}, SweepInterval: time.Hour}
	application, err := newWithFactory(config, slog.Default(), func(Config) (lifecycleBackend, error) { return backend, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := application.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if backend.recoverCalls != 1 || backend.sweepCalls != 1 || backend.closeCalls != 1 {
		t.Fatalf("calls: recover=%d sweep=%d close=%d", backend.recoverCalls, backend.sweepCalls, backend.closeCalls)
	}
}

type fakeLifecycleBackend struct{ recoverCalls, sweepCalls, closeCalls int }

func (b *fakeLifecycleBackend) Acquire(context.Context, lease.Scope, lease.AcquireRequest) (lease.AcquireResult, error) {
	return lease.AcquireResult{}, nil
}
func (b *fakeLifecycleBackend) Get(context.Context, lease.Scope, string) (lease.Record, error) {
	return lease.Record{}, nil
}
func (b *fakeLifecycleBackend) Exec(context.Context, lease.Scope, string, lease.ExecRequest) (lease.ExecResult, error) {
	return lease.ExecResult{}, nil
}
func (b *fakeLifecycleBackend) ReadFile(context.Context, lease.Scope, string, lease.ReadFileRequest) (lease.ReadFileResult, error) {
	return lease.ReadFileResult{}, nil
}
func (b *fakeLifecycleBackend) WriteFile(context.Context, lease.Scope, string, lease.WriteFileRequest) error {
	return nil
}
func (b *fakeLifecycleBackend) Release(context.Context, lease.Scope, string) (lease.Record, error) {
	return lease.Record{}, nil
}
func (b *fakeLifecycleBackend) Delete(context.Context, lease.Scope, string) error { return nil }
func (b *fakeLifecycleBackend) Close(context.Context) error                       { b.closeCalls++; return nil }
func (b *fakeLifecycleBackend) Recover(context.Context) ([]kubernetesbackend.ActiveLease, error) {
	b.recoverCalls++
	return nil, nil
}
func (b *fakeLifecycleBackend) Ready(context.Context) error        { return nil }
func (b *fakeLifecycleBackend) SweepExpired(context.Context) error { b.sweepCalls++; return nil }
