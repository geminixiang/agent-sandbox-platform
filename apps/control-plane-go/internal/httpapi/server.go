package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/auth"
	"github.com/geminixiang/agent-sandbox-platform/apps/control-plane-go/internal/lease"
)

const (
	leasePath            = "/v1/leases"
	maxJSONBodySize      = 1024 * 1024
	MaxFileTransferBytes = lease.MaxFileTransferBytes
)

type Readiness func(context.Context) error

type Server struct {
	backend   lease.Backend
	secrets   auth.SecretResolver
	readiness Readiness
	now       func() time.Time
}

func New(backend lease.Backend, secrets auth.SecretResolver, readiness ...Readiness) http.Handler {
	check := Readiness(func(context.Context) error { return nil })
	if len(readiness) > 0 && readiness[0] != nil {
		check = readiness[0]
	}
	return &Server{backend: backend, secrets: secrets, readiness: check, now: time.Now}
}

func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/health" {
		writeJSON(response, 200, map[string]string{"status": "ok"})
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/ready" {
		ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
		defer cancel()
		if err := s.readiness(ctx); err != nil {
			writeJSON(response, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		writeJSON(response, 200, map[string]string{"status": "ready"})
		return
	}
	scope, err := s.authenticate(request)
	if err != nil {
		writeError(response, err)
		return
	}

	if request.URL.Path == leasePath {
		if request.Method != http.MethodPost {
			writeError(response, lease.NewError(405, "METHOD_NOT_ALLOWED", "Method not allowed"))
			return
		}
		var body lease.AcquireRequest
		if err = readJSON(request, &body); err != nil {
			writeError(response, err)
			return
		}
		if strings.TrimSpace(body.Pool) == "" {
			writeError(response, invalidString("pool"))
			return
		}
		body.IdempotencyKey, err = requiredHeader(request, "Idempotency-Key")
		if err != nil {
			writeError(response, err)
			return
		}
		result, err := s.backend.Acquire(request.Context(), scope, body)
		if err != nil {
			writeError(response, err)
			return
		}
		writeJSON(response, 201, result)
		return
	}

	id, action, ok := matchLeaseRoute(request.URL.Path)
	if !ok {
		writeError(response, lease.NewError(404, "NOT_FOUND", "Route not found"))
		return
	}
	ctx := request.Context()
	switch {
	case request.Method == http.MethodGet && action == "":
		value, backendErr := s.backend.Get(ctx, scope, id)
		if backendErr != nil {
			writeError(response, backendErr)
			return
		}
		writeJSON(response, 200, map[string]lease.Record{"lease": value})
	case request.Method == http.MethodDelete && action == "":
		if err = s.backend.Delete(ctx, scope, id); err != nil {
			writeError(response, err)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodPost && action == "exec":
		var body lease.ExecRequest
		if err = readJSON(request, &body); err != nil {
			writeError(response, err)
			return
		}
		if strings.TrimSpace(body.Command) == "" {
			writeError(response, invalidString("command"))
			return
		}
		value, backendErr := s.backend.Exec(ctx, scope, id, body)
		if backendErr != nil {
			writeError(response, backendErr)
			return
		}
		writeJSON(response, 200, value)
	case request.Method == http.MethodPost && action == "files/read":
		var body lease.ReadFileRequest
		if err = readJSON(request, &body); err != nil {
			writeError(response, err)
			return
		}
		if strings.TrimSpace(body.Path) == "" {
			writeError(response, invalidString("path"))
			return
		}
		if err = requireEncoding(body.Encoding); err != nil {
			writeError(response, err)
			return
		}
		value, backendErr := s.backend.ReadFile(ctx, scope, id, body)
		if backendErr != nil {
			writeError(response, backendErr)
			return
		}
		writeJSON(response, 200, value)
	case request.Method == http.MethodPost && action == "files/write":
		var body lease.WriteFileRequest
		if err = readJSON(request, &body); err != nil {
			writeError(response, err)
			return
		}
		if strings.TrimSpace(body.Path) == "" {
			writeError(response, invalidString("path"))
			return
		}
		if err = requireEncoding(body.Encoding); err != nil {
			writeError(response, err)
			return
		}
		if err = s.backend.WriteFile(ctx, scope, id, body); err != nil {
			writeError(response, err)
			return
		}
		writeJSON(response, 200, map[string]string{"path": body.Path})
	case request.Method == http.MethodGet && action == "files/content":
		s.readFileContent(response, request, scope, id)
	case request.Method == http.MethodPut && action == "files/content":
		s.writeFileContent(response, request, scope, id)
	case request.Method == http.MethodPost && action == "release":
		value, backendErr := s.backend.Release(ctx, scope, id)
		if backendErr != nil {
			writeError(response, backendErr)
			return
		}
		writeJSON(response, 200, map[string]lease.Record{"lease": value})
	default:
		writeError(response, lease.NewError(405, "METHOD_NOT_ALLOWED", "Method not allowed"))
	}
}

func (s *Server) readFileContent(response http.ResponseWriter, request *http.Request, scope lease.Scope, id string) {
	path, err := requiredFilePath(request)
	if err != nil {
		writeError(response, err)
		return
	}
	backend, ok := s.backend.(lease.FileTransferBackend)
	if !ok {
		writeError(response, streamingNotSupported())
		return
	}
	download, err := backend.OpenFile(request.Context(), scope, id, path)
	if err != nil {
		writeError(response, err)
		return
	}
	if download.Content == nil {
		writeError(response, lease.NewError(500, "INTERNAL_ERROR", "Internal server error"))
		return
	}
	defer download.Content.Close()
	if download.SizeBytes < 0 {
		writeError(response, lease.NewError(500, "INTERNAL_ERROR", "Internal server error"))
		return
	}
	if download.SizeBytes > MaxFileTransferBytes {
		writeError(response, transferTooLarge())
		return
	}

	response.Header().Set("Content-Type", "application/octet-stream")
	response.Header().Set("Content-Length", strconv.FormatInt(download.SizeBytes, 10))
	response.Header().Set("Content-Digest", formatContentDigest(download.SHA256))
	response.WriteHeader(http.StatusOK)

	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(response, digest), io.LimitReader(download.Content, download.SizeBytes+1))
	if copyErr != nil || written != download.SizeBytes || !equalDigest(digest, download.SHA256) {
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) writeFileContent(response http.ResponseWriter, request *http.Request, scope lease.Scope, id string) {
	path, err := requiredFilePath(request)
	if err != nil {
		writeError(response, err)
		return
	}
	mediaType, _, mediaErr := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/octet-stream" {
		writeError(response, lease.NewError(415, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/octet-stream"))
		return
	}
	if request.ContentLength < 0 {
		writeError(response, lease.NewError(411, "LENGTH_REQUIRED", "Content-Length is required"))
		return
	}
	if request.ContentLength > MaxFileTransferBytes {
		writeError(response, transferTooLarge())
		return
	}
	digest, err := parseContentDigest(request.Header.Get("Content-Digest"))
	if err != nil {
		writeError(response, err)
		return
	}
	backend, ok := s.backend.(lease.FileTransferBackend)
	if !ok {
		writeError(response, streamingNotSupported())
		return
	}

	limited := http.MaxBytesReader(response, request.Body, request.ContentLength)
	tracked := &transferReader{reader: limited, digest: sha256.New()}
	err = backend.WriteFileStream(request.Context(), scope, id, lease.StreamWriteRequest{
		Path: path, SizeBytes: request.ContentLength, SHA256: digest,
	}, tracked)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(response, lease.NewError(400, "CONTENT_LENGTH_MISMATCH", "Request body does not match Content-Length"))
			return
		}
		writeError(response, err)
		return
	}
	if tracked.count != request.ContentLength {
		writeError(response, lease.NewError(400, "CONTENT_LENGTH_MISMATCH", "Request body does not match Content-Length"))
		return
	}
	if !equalDigest(tracked.digest, digest) {
		writeError(response, lease.NewError(422, "CONTENT_DIGEST_MISMATCH", "Request body does not match Content-Digest"))
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) authenticate(request *http.Request) (lease.Scope, error) {
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return lease.Scope{}, lease.NewError(401, "UNAUTHORIZED", "Invalid or expired subject token")
	}
	return auth.VerifySubjectToken(strings.TrimPrefix(value, "Bearer "), s.secrets, s.now(), 5*time.Minute)
}

func matchLeaseRoute(path string) (string, string, bool) {
	if !strings.HasPrefix(path, leasePath+"/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, leasePath+"/"), "/")
	id, err := url.PathUnescape(parts[0])
	if err != nil || id == "" {
		return "", "", false
	}
	for index := 1; index < len(parts); index++ {
		parts[index], _ = url.PathUnescape(parts[index])
	}
	return id, strings.Join(parts[1:], "/"), true
}

func readJSON(request *http.Request, target any) error {
	reader := http.MaxBytesReader(nil, request.Body, maxJSONBodySize)
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return lease.NewError(413, "BODY_TOO_LARGE", "JSON body is too large")
		}
		return lease.NewError(400, "INVALID_JSON", "Request body must be valid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return lease.NewError(400, "INVALID_JSON", "Request body must be valid JSON")
	}
	return nil
}

func requiredHeader(request *http.Request, name string) (string, error) {
	value := request.Header.Get(name)
	if strings.TrimSpace(value) == "" || len(value) > 200 {
		return "", lease.NewError(400, "INVALID_REQUEST", "'idempotency-key' header is required")
	}
	return value, nil
}
func requireEncoding(value string) error {
	if value != "" && value != "utf8" && value != "base64" {
		return lease.NewError(400, "INVALID_REQUEST", "'encoding' must be 'utf8' or 'base64'")
	}
	return nil
}
func invalidString(field string) error {
	return lease.NewError(400, "INVALID_REQUEST", "'"+field+"' must be a non-empty string")
}

func requiredFilePath(request *http.Request) (string, error) {
	values, ok := request.URL.Query()["path"]
	if !ok || len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return "", invalidString("path")
	}
	return values[0], nil
}

func parseContentDigest(value string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	const prefix = "sha-256=:"
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, ":") || strings.Count(value, ":") != 2 {
		return result, lease.NewError(400, "INVALID_CONTENT_DIGEST", "Content-Digest must contain one canonical sha-256 digest")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSuffix(strings.TrimPrefix(value, prefix), ":"))
	if err != nil || len(decoded) != sha256.Size {
		return result, lease.NewError(400, "INVALID_CONTENT_DIGEST", "Content-Digest must contain one canonical sha-256 digest")
	}
	copy(result[:], decoded)
	if formatContentDigest(result) != value {
		return [sha256.Size]byte{}, lease.NewError(400, "INVALID_CONTENT_DIGEST", "Content-Digest must contain one canonical sha-256 digest")
	}
	return result, nil
}

func formatContentDigest(value [sha256.Size]byte) string {
	return fmt.Sprintf("sha-256=:%s:", base64.StdEncoding.EncodeToString(value[:]))
}

func equalDigest(actual hash.Hash, expected [sha256.Size]byte) bool {
	return string(actual.Sum(nil)) == string(expected[:])
}

func streamingNotSupported() error {
	return lease.NewError(501, "STREAMING_NOT_SUPPORTED", "Streaming file transfer is not supported by this backend")
}

func transferTooLarge() error {
	return lease.NewError(413, "TRANSFER_TOO_LARGE", "File transfer exceeds the 64 MiB limit")
}

type transferReader struct {
	reader io.Reader
	digest hash.Hash
	count  int64
}

func (r *transferReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if n > 0 {
		r.count += int64(n)
		_, _ = r.digest.Write(buffer[:n])
	}
	return n, err
}

func writeError(response http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		err = lease.NewError(408, "ABORTED", "Request was aborted")
	}
	status, code, message := lease.ErrorDetails(err)
	writeJSON(response, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func writeJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}
