package sandbox

import (
	"io"
	"time"
)

const (
	// Version is the SDK release version.
	Version = "0.2.0-rc.1"
	// MaxFileTransferBytes is the protocol streaming transfer limit.
	MaxFileTransferBytes int64 = 64 * 1024 * 1024
)

type LeaseStatus string

const (
	LeaseActive   LeaseStatus = "active"
	LeaseReleased LeaseStatus = "released"
	LeaseExpired  LeaseStatus = "expired"
)

// LeaseRecord is an immutable-by-convention snapshot returned by the platform.
type LeaseRecord struct {
	ID         string      `json:"id"`
	Pool       string      `json:"pool"`
	Status     LeaseStatus `json:"status"`
	CreatedAt  time.Time   `json:"createdAt"`
	ExpiresAt  time.Time   `json:"expiresAt"`
	LastUsedAt time.Time   `json:"lastUsedAt"`
}

type CreateOptions struct {
	Pool           string
	TTLSeconds     int
	IdempotencyKey string
}

type ListOptions struct {
	Pool   string
	Limit  int
	Cursor string
}

type AcquireResult struct {
	Sandbox        *Sandbox
	Replayed       bool
	IdempotencyKey string
}

type Page struct {
	Sandboxes  []*Sandbox
	NextCursor string
}

type RunOptions struct {
	CWD            string
	Env            map[string]string
	TimeoutSeconds int
	Check          bool
}

type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (r CommandResult) Succeeded() bool { return r.ExitCode == 0 }

type UploadOptions struct {
	SizeBytes int64
	SHA256    string
}

// Download is a verified streaming response. Read verifies exact length and
// digest at normal EOF. Closing early cancels without verifying.
type Download struct {
	Content   io.ReadCloser
	SizeBytes int64
	SHA256    string
}
