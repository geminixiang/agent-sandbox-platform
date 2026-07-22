package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/auth"
	kubernetesbackend "github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/backend/kubernetes"
	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/httpapi"
	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

type Config struct {
	Address                   string
	Kubeconfig                string
	KubeContext               string
	Namespace                 string
	MetadataSecret            string
	ConsumerSecrets           map[string]string
	Pools                     map[string]kubernetesbackend.Pool
	DefaultTTLSeconds         int
	MaxTTLSeconds             int
	ReadyTimeout              time.Duration
	PollInterval              time.Duration
	SweepInterval             time.Duration
	FileTransferMaxConcurrent int
	FileTransferMaxPerLease   int
	FileTransferTimeout       time.Duration
}

type lifecycleBackend interface {
	httpapiBackend
	Recover(context.Context) ([]kubernetesbackend.ActiveLease, error)
	SweepExpired(context.Context) error
	Ready(context.Context) error
}

type httpapiBackend interface {
	Acquire(context.Context, lease.Scope, lease.AcquireRequest) (lease.AcquireResult, error)
	List(context.Context, lease.Scope, lease.ListRequest) (lease.Page, error)
	Get(context.Context, lease.Scope, string) (lease.Record, error)
	Exec(context.Context, lease.Scope, string, lease.ExecRequest) (lease.ExecResult, error)
	ReadFile(context.Context, lease.Scope, string, lease.ReadFileRequest) (lease.ReadFileResult, error)
	WriteFile(context.Context, lease.Scope, string, lease.WriteFileRequest) error
	Release(context.Context, lease.Scope, string) (lease.Record, error)
	Delete(context.Context, lease.Scope, string) error
	Close(context.Context) error
}

type backendFactory func(Config) (lifecycleBackend, error)

type App struct {
	server        *http.Server
	backend       lifecycleBackend
	sweepInterval time.Duration
	logger        *slog.Logger
}

func LoadConfig(getenv func(string) string) (Config, error) {
	config := Config{
		Address:                   env(getenv, "SANDBOX_ADDRESS", "127.0.0.1:8787"),
		Kubeconfig:                getenv("SANDBOX_KUBECONFIG"),
		KubeContext:               getenv("SANDBOX_K8S_CONTEXT"),
		Namespace:                 getenv("SANDBOX_K8S_NAMESPACE"),
		MetadataSecret:            getenv("SANDBOX_METADATA_SECRET"),
		DefaultTTLSeconds:         900,
		MaxTTLSeconds:             3600,
		ReadyTimeout:              2 * time.Minute,
		PollInterval:              500 * time.Millisecond,
		SweepInterval:             30 * time.Second,
		FileTransferMaxConcurrent: 8,
		FileTransferMaxPerLease:   2,
		FileTransferTimeout:       2 * time.Minute,
	}
	if strings.TrimSpace(config.Namespace) == "" {
		return Config{}, errors.New("SANDBOX_K8S_NAMESPACE is required")
	}
	if config.MetadataSecret == "" {
		return Config{}, errors.New("SANDBOX_METADATA_SECRET is required")
	}
	if err := decodeMap(getenv("SANDBOX_CONSUMER_SECRETS"), "SANDBOX_CONSUMER_SECRETS", &config.ConsumerSecrets); err != nil {
		return Config{}, err
	}
	if err := decodeMap(getenv("SANDBOX_K8S_POOLS"), "SANDBOX_K8S_POOLS", &config.Pools); err != nil {
		return Config{}, err
	}
	var err error
	if config.DefaultTTLSeconds, err = positiveInt(getenv, "SANDBOX_DEFAULT_TTL_SECONDS", config.DefaultTTLSeconds); err != nil {
		return Config{}, err
	}
	if config.MaxTTLSeconds, err = positiveInt(getenv, "SANDBOX_MAX_TTL_SECONDS", config.MaxTTLSeconds); err != nil {
		return Config{}, err
	}
	if config.ReadyTimeout, err = positiveDuration(getenv, "SANDBOX_K8S_READY_TIMEOUT", config.ReadyTimeout); err != nil {
		return Config{}, err
	}
	if config.PollInterval, err = positiveDuration(getenv, "SANDBOX_K8S_POLL_INTERVAL", config.PollInterval); err != nil {
		return Config{}, err
	}
	if config.SweepInterval, err = positiveDuration(getenv, "SANDBOX_SWEEP_INTERVAL", config.SweepInterval); err != nil {
		return Config{}, err
	}
	if config.FileTransferMaxConcurrent, err = positiveInt(getenv, "SANDBOX_FILE_TRANSFER_MAX_CONCURRENT", config.FileTransferMaxConcurrent); err != nil {
		return Config{}, err
	}
	if config.FileTransferMaxPerLease, err = positiveInt(getenv, "SANDBOX_FILE_TRANSFER_MAX_PER_LEASE", config.FileTransferMaxPerLease); err != nil {
		return Config{}, err
	}
	if config.FileTransferMaxPerLease > config.FileTransferMaxConcurrent {
		return Config{}, errors.New("SANDBOX_FILE_TRANSFER_MAX_PER_LEASE must not exceed SANDBOX_FILE_TRANSFER_MAX_CONCURRENT")
	}
	if config.FileTransferTimeout, err = positiveDuration(getenv, "SANDBOX_FILE_TRANSFER_TIMEOUT", config.FileTransferTimeout); err != nil {
		return Config{}, err
	}
	return config, nil
}

func New(config Config, logger *slog.Logger) (*App, error) {
	return newWithFactory(config, logger, newKubernetesBackend)
}

func newWithFactory(config Config, logger *slog.Logger, factory backendFactory) (*App, error) {
	backend, err := factory(config)
	if err != nil {
		return nil, err
	}
	resolver := auth.SecretResolver(func(consumerID string) (string, bool) {
		secret, ok := config.ConsumerSecrets[consumerID]
		return secret, ok
	})
	return &App{server: &http.Server{Addr: config.Address, Handler: httpapi.New(backend, resolver, backend.Ready), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}, backend: backend, sweepInterval: config.SweepInterval, logger: logger}, nil
}

func newKubernetesBackend(config Config) (lifecycleBackend, error) {
	restConfig, err := kubernetesbackend.LoadConfig(config.Kubeconfig, config.KubeContext)
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	return kubernetesbackend.NewFromConfig(restConfig, kubernetesbackend.Options{Namespace: config.Namespace, MetadataSecret: config.MetadataSecret, Pools: config.Pools, DefaultTTLSeconds: config.DefaultTTLSeconds, MaxTTLSeconds: config.MaxTTLSeconds, ReadyTimeout: config.ReadyTimeout, PollInterval: config.PollInterval, MaxTransfers: config.FileTransferMaxConcurrent, MaxTransfersPerLease: config.FileTransferMaxPerLease, TransferTimeout: config.FileTransferTimeout})
}

func (a *App) Run(ctx context.Context) error {
	if _, err := a.backend.Recover(ctx); err != nil {
		return fmt.Errorf("recover leases: %w", err)
	}
	if err := a.backend.SweepExpired(ctx); err != nil {
		return fmt.Errorf("sweep expired leases: %w", err)
	}
	listener, err := net.Listen("tcp", a.server.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- a.server.Serve(listener) }()
	ticker := time.NewTicker(a.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := a.server.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shutdown HTTP server: %w", err)
			}
			return a.backend.Close(shutdownCtx)
		case err := <-serveErrors:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("serve HTTP: %w", err)
		case <-ticker.C:
			if err := a.backend.SweepExpired(ctx); err != nil {
				a.logger.Error("Lease expiry sweep failed", "error", err)
			}
		}
	}
}

func env(getenv func(string) string, name, fallback string) string {
	if value := getenv(name); value != "" {
		return value
	}
	return fallback
}
func decodeMap[T any](raw, name string, target *map[string]T) error {
	if raw == "" {
		return fmt.Errorf("%s is required", name)
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil || *target == nil || len(*target) == 0 {
		return fmt.Errorf("%s must be a non-empty JSON object", name)
	}
	return nil
}
func positiveInt(getenv func(string) string, name string, fallback int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}
func positiveDuration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}
