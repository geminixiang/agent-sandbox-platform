package lease

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"time"
)

const WorkspacePath = "/workspace"

type Status string

const (
	StatusActive   Status = "active"
	StatusReleased Status = "released"
	StatusExpired  Status = "expired"
)

type Scope struct {
	ConsumerID string
	SubjectID  string
}

type Record struct {
	ID         string    `json:"id"`
	Pool       string    `json:"pool"`
	Status     Status    `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
}

type AcquireRequest struct {
	Pool           string `json:"pool"`
	TTLSeconds     *int   `json:"ttlSeconds,omitempty"`
	IdempotencyKey string `json:"-"`
}

type AcquireResult struct {
	Lease    Record `json:"lease"`
	Replayed bool   `json:"replayed"`
}

type ExecRequest struct {
	Command        string            `json:"command"`
	CWD            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds *int              `json:"timeoutSeconds,omitempty"`
}

type ExecResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Code   int    `json:"code"`
}

type ReadFileRequest struct {
	Path     string `json:"path"`
	Encoding string `json:"encoding,omitempty"`
}

type ReadFileResult struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

type WriteFileRequest struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty"`
}

// FileDownload describes stable content whose size and digest were determined
// before the HTTP response is committed.
type FileDownload struct {
	Content   io.ReadCloser
	SizeBytes int64
	SHA256    [sha256.Size]byte
}

// StreamWriteRequest contains the metadata a streaming backend must verify
// before atomically replacing the destination file.
type StreamWriteRequest struct {
	Path      string
	SizeBytes int64
	SHA256    [sha256.Size]byte
}

// FileTransferBackend is an optional backend capability. Implementations must
// preflight downloads and atomically verify streamed writes without buffering
// an entire transfer in the control plane.
type FileTransferBackend interface {
	OpenFile(context.Context, Scope, string, string) (FileDownload, error)
	WriteFileStream(context.Context, Scope, string, StreamWriteRequest, io.Reader) error
}

type Backend interface {
	Acquire(context.Context, Scope, AcquireRequest) (AcquireResult, error)
	Get(context.Context, Scope, string) (Record, error)
	Exec(context.Context, Scope, string, ExecRequest) (ExecResult, error)
	ReadFile(context.Context, Scope, string, ReadFileRequest) (ReadFileResult, error)
	WriteFile(context.Context, Scope, string, WriteFileRequest) error
	Release(context.Context, Scope, string) (Record, error)
	Delete(context.Context, Scope, string) error
	Close(context.Context) error
}

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func NewError(status int, code, message string) error {
	return &Error{Status: status, Code: code, Message: message}
}

func ErrorDetails(err error) (status int, code, message string) {
	var leaseErr *Error
	if errors.As(err, &leaseErr) {
		return leaseErr.Status, leaseErr.Code, leaseErr.Message
	}
	return 500, "INTERNAL_ERROR", "Internal server error"
}

func NotFound() error { return NewError(404, "LEASE_NOT_FOUND", "Lease not found") }
