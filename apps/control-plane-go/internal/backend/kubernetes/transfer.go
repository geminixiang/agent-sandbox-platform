package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

const (
	transferHelper           = "/usr/local/bin/agent-sandbox-transfer"
	maxTransferMarkerBytes   = 256
	maxTransferStderrBytes   = 4096
	defaultGlobalTransfers   = 8
	defaultPerLeaseTransfers = 2
	defaultTransferTimeout   = 2 * time.Minute
)

type transferManager struct {
	mu          sync.Mutex
	closed      bool
	blocked     map[string]bool
	active      map[string]map[*transferOperation]struct{}
	global      int
	perLease    map[string]int
	maxGlobal   int
	maxPerLease int
	timeout     time.Duration
	wg          sync.WaitGroup
}

type transferOperation struct {
	manager *transferManager
	leaseID string
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

func newTransferManager(maxGlobal, maxPerLease int, timeout time.Duration) *transferManager {
	return &transferManager{blocked: map[string]bool{}, active: map[string]map[*transferOperation]struct{}{}, perLease: map[string]int{}, maxGlobal: maxGlobal, maxPerLease: maxPerLease, timeout: timeout}
}

func (m *transferManager) begin(parent context.Context, leaseID string, expiresAt time.Time) (*transferOperation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.blocked[leaseID] {
		return nil, lease.NewError(408, "ABORTED", "File transfer was aborted")
	}
	if m.global >= m.maxGlobal || m.perLease[leaseID] >= m.maxPerLease {
		return nil, lease.NewError(429, "TRANSFER_LIMIT_REACHED", "File transfer concurrency limit reached")
	}
	deadline := time.Now().Add(m.timeout)
	if expiresAt.Before(deadline) {
		deadline = expiresAt
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	op := &transferOperation{manager: m, leaseID: leaseID, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	if m.active[leaseID] == nil {
		m.active[leaseID] = map[*transferOperation]struct{}{}
	}
	m.active[leaseID][op] = struct{}{}
	m.global++
	m.perLease[leaseID]++
	m.wg.Add(1)
	return op, nil
}

func (op *transferOperation) finish() {
	op.once.Do(func() {
		op.cancel()
		m := op.manager
		m.mu.Lock()
		delete(m.active[op.leaseID], op)
		if len(m.active[op.leaseID]) == 0 {
			delete(m.active, op.leaseID)
		}
		m.global--
		m.perLease[op.leaseID]--
		if m.perLease[op.leaseID] == 0 {
			delete(m.perLease, op.leaseID)
		}
		m.mu.Unlock()
		m.wg.Done()
		close(op.done)
	})
}

func (m *transferManager) stopLease(ctx context.Context, leaseID string) error {
	m.mu.Lock()
	m.blocked[leaseID] = true
	operations := make([]*transferOperation, 0, len(m.active[leaseID]))
	for op := range m.active[leaseID] {
		operations = append(operations, op)
		op.cancel()
	}
	m.mu.Unlock()
	return waitTransfers(ctx, operations)
}

func (m *transferManager) close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	for _, leaseOperations := range m.active {
		for op := range leaseOperations {
			op.cancel()
		}
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return lease.NewError(408, "ABORTED", "File transfer was aborted")
	}
}

func waitTransfers(ctx context.Context, operations []*transferOperation) error {
	for _, op := range operations {
		select {
		case <-op.done:
		case <-ctx.Done():
			return lease.NewError(408, "ABORTED", "File transfer was aborted")
		}
	}
	return nil
}

func (b *Backend) OpenFile(ctx context.Context, scope lease.Scope, id, requestedPath string) (lease.FileDownload, error) {
	target, expiresAt, path, err := b.prepareTransfer(ctx, scope, id, requestedPath)
	if err != nil {
		return lease.FileDownload{}, err
	}
	op, err := b.transfers.begin(ctx, id, expiresAt)
	if err != nil {
		return lease.FileDownload{}, err
	}
	stdoutReader, stdoutWriter := io.Pipe()
	stderr := &boundedBuffer{limit: maxTransferStderrBytes}
	go func() {
		runErr := b.runner.Run(op.ctx, b.namespace, target, []string{transferHelper, "download", path}, nil, stdoutWriter, stderr)
		_ = stdoutWriter.CloseWithError(mapTransferRunError(op.ctx, runErr))
		op.finish()
	}()

	marker, markerErr := readTransferMarker(stdoutReader)
	if markerErr == nil && marker.code != "" {
		op.cancel()
		_ = stdoutReader.Close()
		<-op.done
		return lease.FileDownload{}, remoteTransferError(marker.code)
	}
	if op.ctx.Err() != nil {
		op.cancel()
		_ = stdoutReader.Close()
		<-op.done
		return lease.FileDownload{}, lease.NewError(408, "ABORTED", "File transfer was aborted")
	}
	if markerErr != nil {
		op.cancel()
		_ = stdoutReader.Close()
		<-op.done
		return lease.FileDownload{}, normalizeTransferError(markerErr)
	}
	if marker.size < 0 || marker.size > lease.MaxFileTransferBytes {
		op.cancel()
		_ = stdoutReader.Close()
		<-op.done
		return lease.FileDownload{}, lease.NewError(413, "TRANSFER_TOO_LARGE", "File transfer exceeds the 64 MiB limit")
	}
	return lease.FileDownload{Content: &transferDownloadReader{reader: stdoutReader, op: op}, SizeBytes: marker.size, SHA256: marker.digest}, nil
}

func (b *Backend) WriteFileStream(ctx context.Context, scope lease.Scope, id string, request lease.StreamWriteRequest, content io.Reader) error {
	if request.SizeBytes < 0 || request.SizeBytes > lease.MaxFileTransferBytes {
		return lease.NewError(413, "TRANSFER_TOO_LARGE", "File transfer exceeds the 64 MiB limit")
	}
	target, expiresAt, path, err := b.prepareTransfer(ctx, scope, id, request.Path)
	if err != nil {
		return err
	}
	op, err := b.transfers.begin(ctx, id, expiresAt)
	if err != nil {
		return err
	}
	defer op.finish()
	stdout := &boundedBuffer{limit: maxTransferMarkerBytes}
	stderr := &boundedBuffer{limit: maxTransferStderrBytes}
	command := []string{transferHelper, "upload", path, strconv.FormatInt(request.SizeBytes, 10), hex.EncodeToString(request.SHA256[:])}
	input := &uploadInput{reader: content}
	runErr := b.runner.Run(op.ctx, b.namespace, target, command, input, stdout, stderr)
	if op.ctx.Err() != nil {
		return lease.NewError(408, "ABORTED", "File transfer was aborted")
	}
	if input.err != nil && !errors.Is(input.err, io.EOF) && input.count >= request.SizeBytes {
		return lease.NewError(400, "CONTENT_LENGTH_MISMATCH", "Request body does not match Content-Length")
	}
	if !stdout.exceeded {
		marker, markerErr := parseTransferMarker(strings.TrimSuffix(stdout.String(), "\n"))
		if markerErr == nil {
			if marker.code != "" {
				return remoteTransferError(marker.code)
			}
			if runErr == nil {
				return nil
			}
		}
	}
	if err := mapTransferRunError(op.ctx, runErr); err != nil {
		return err
	}
	return lease.NewError(502, "FILE_TRANSFER_FAILED", "Workspace file transfer failed")
}

func (b *Backend) stopLeaseAndDelete(ctx context.Context, id string) error {
	if err := b.transfers.stopLease(ctx, id); err != nil {
		return err
	}
	return b.deleteClaim(ctx, id)
}

func (b *Backend) prepareTransfer(ctx context.Context, scope lease.Scope, id, requestedPath string) (podTarget, time.Time, string, error) {
	claim, err := b.requireClaim(ctx, scope, id)
	if err != nil {
		return podTarget{}, time.Time{}, "", err
	}
	record, err := recordFromClaim(claim)
	if err != nil {
		return podTarget{}, time.Time{}, "", err
	}
	if !record.ExpiresAt.After(b.now()) {
		_ = b.stopLeaseAndDelete(context.WithoutCancel(ctx), id)
		return podTarget{}, time.Time{}, "", lease.NewError(409, "LEASE_NOT_ACTIVE", "Lease is not active")
	}
	path, err := normalizeWorkspacePath(requestedPath)
	if err != nil || path == lease.WorkspacePath {
		return podTarget{}, time.Time{}, "", lease.NewError(400, "INVALID_PATH", "Path must name a file inside /workspace")
	}
	target, err := b.targetForClaim(ctx, claim, record.Pool)
	return target, record.ExpiresAt, path, err
}

type transferMarker struct {
	size   int64
	digest [sha256.Size]byte
	code   string
}

func readTransferMarker(reader io.Reader) (transferMarker, error) {
	buffer := make([]byte, 0, maxTransferMarkerBytes)
	one := []byte{0}
	for len(buffer) < maxTransferMarkerBytes {
		n, err := reader.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return parseTransferMarker(string(buffer))
			}
			buffer = append(buffer, one[0])
		}
		if err != nil {
			return transferMarker{}, err
		}
	}
	return transferMarker{}, errors.New("transfer marker exceeds limit")
}

func parseTransferMarker(value string) (transferMarker, error) {
	parts := strings.Split(value, " ")
	if len(parts) == 3 && parts[0] == "ASP1" && parts[1] == "ERR" && parts[2] != "" {
		return transferMarker{code: parts[2]}, nil
	}
	if len(parts) == 2 && parts[0] == "ASP1" && parts[1] == "OK" {
		return transferMarker{}, nil
	}
	if len(parts) == 4 && parts[0] == "ASP1" && parts[1] == "OK" {
		size, err := strconv.ParseInt(parts[2], 10, 64)
		digest, digestErr := hex.DecodeString(parts[3])
		if err == nil && digestErr == nil && len(digest) == sha256.Size {
			var value [sha256.Size]byte
			copy(value[:], digest)
			return transferMarker{size: size, digest: value}, nil
		}
	}
	return transferMarker{}, errors.New("invalid transfer marker")
}

func remoteTransferError(code string) error {
	switch code {
	case "FILE_NOT_FOUND":
		return lease.NewError(404, code, "Workspace file not found")
	case "INVALID_PATH":
		return lease.NewError(400, code, "Path must name a regular file inside /workspace")
	case "TRANSFER_TOO_LARGE":
		return lease.NewError(413, code, "File transfer exceeds the 64 MiB limit")
	case "CONTENT_LENGTH_MISMATCH":
		return lease.NewError(400, code, "Request body does not match Content-Length")
	case "CONTENT_DIGEST_MISMATCH":
		return lease.NewError(422, code, "Request body does not match Content-Digest")
	default:
		return lease.NewError(502, "FILE_TRANSFER_FAILED", "Workspace file transfer failed")
	}
}

func normalizeTransferError(err error) error {
	if err == nil {
		return nil
	}
	var typed *lease.Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return lease.NewError(408, "ABORTED", "File transfer was aborted")
	}
	return lease.NewError(502, "FILE_TRANSFER_FAILED", "Workspace file transfer failed")
}

func mapTransferRunError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return lease.NewError(408, "ABORTED", "File transfer was aborted")
	}
	if err != nil {
		return lease.NewError(502, "FILE_TRANSFER_FAILED", "Workspace file transfer failed")
	}
	return nil
}

type transferDownloadReader struct {
	reader *io.PipeReader
	op     *transferOperation
	once   sync.Once
}

func (r *transferDownloadReader) Read(value []byte) (int, error) { return r.reader.Read(value) }
func (r *transferDownloadReader) Close() error {
	var closeErr error
	r.once.Do(func() {
		r.op.cancel()
		closeErr = r.reader.Close()
		<-r.op.done
	})
	return closeErr
}

type uploadInput struct {
	reader io.Reader
	count  int64
	err    error
}

func (r *uploadInput) Read(value []byte) (int, error) {
	n, err := r.reader.Read(value)
	r.count += int64(n)
	if err != nil {
		r.err = err
	}
	return n, err
}

type boundedBuffer struct {
	mu       sync.Mutex
	value    []byte
	limit    int
	exceeded bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	available := b.limit - len(b.value)
	if available > 0 {
		b.value = append(b.value, value[:min(available, len(value))]...)
	}
	if len(value) > available {
		b.exceeded = true
	}
	return len(value), nil
}
func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.value)
}

var _ lease.FileTransferBackend = (*Backend)(nil)
