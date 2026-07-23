package sandbox

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (f *Files) WriteStream(ctx context.Context, path string, source io.Reader, options UploadOptions) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("sandbox: path must not be blank")
	}
	if source == nil {
		return errors.New("sandbox: source is required")
	}
	if options.SizeBytes < 0 {
		return errors.New("sandbox: SizeBytes must not be negative")
	}
	if options.SizeBytes > MaxFileTransferBytes {
		return &Error{Code: "TRANSFER_TOO_LARGE", Message: "sandbox: file transfer exceeds 64 MiB"}
	}
	if !sha256Hex.MatchString(options.SHA256) {
		return errors.New("sandbox: SHA256 must be lowercase 64-character hexadecimal")
	}
	digestBytes, _ := hex.DecodeString(options.SHA256)
	digestHeader := "sha-256=:" + base64.StdEncoding.EncodeToString(digestBytes) + ":"
	requestPath := leasePath + "/" + url.PathEscape(f.sandbox.ID()) + "/files/content"
	request, err := f.sandbox.client.newRequest(ctx, http.MethodPut, requestPath, url.Values{"path": {path}}, io.NopCloser(io.LimitReader(source, options.SizeBytes+1)))
	if err != nil {
		return err
	}
	request.ContentLength = options.SizeBytes
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("Content-Digest", digestHeader)
	response, err := f.sandbox.client.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return abortedError(ctx.Err())
		}
		return operationError(err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return decodePlatformError(response)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusNoContent {
		return &Error{StatusCode: response.StatusCode, Code: codeInvalidResponse, Message: "sandbox: unexpected streaming upload response"}
	}
	return nil
}

func (f *Files) ReadStream(ctx context.Context, path string) (*Download, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("sandbox: path must not be blank")
	}
	requestPath := leasePath + "/" + url.PathEscape(f.sandbox.ID()) + "/files/content"
	request, err := f.sandbox.client.newRequest(ctx, http.MethodGet, requestPath, url.Values{"path": {path}}, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	response, err := f.sandbox.client.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, abortedError(ctx.Err())
		}
		return nil, operationError(err)
	}
	if response.StatusCode >= 400 {
		defer response.Body.Close()
		return nil, decodePlatformError(response)
	}
	if response.StatusCode != http.StatusOK || strings.Split(response.Header.Get("Content-Type"), ";")[0] != "application/octet-stream" {
		response.Body.Close()
		return nil, &Error{Code: codeInvalidResponse, Message: "sandbox: invalid streaming download response"}
	}
	size, err := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 {
		response.Body.Close()
		return nil, &Error{Code: codeInvalidResponse, Message: "sandbox: invalid Content-Length"}
	}
	if size > MaxFileTransferBytes {
		response.Body.Close()
		return nil, &Error{Code: "TRANSFER_TOO_LARGE", Message: "sandbox: file transfer exceeds 64 MiB"}
	}
	digest, err := parseContentDigest(response.Header.Get("Content-Digest"))
	if err != nil {
		response.Body.Close()
		return nil, err
	}
	verified := &verifiedReader{body: response.Body, expectedSize: size, expectedDigest: digest, digest: sha256.New()}
	return &Download{Content: verified, SizeBytes: size, SHA256: hex.EncodeToString(digest[:])}, nil
}

func parseContentDigest(value string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	if !strings.HasPrefix(value, "sha-256=:") || !strings.HasSuffix(value, ":") {
		return result, integrityError("sandbox: invalid Content-Digest", nil)
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSuffix(strings.TrimPrefix(value, "sha-256=:"), ":"))
	if err != nil || len(decoded) != sha256.Size {
		return result, integrityError("sandbox: invalid Content-Digest", err)
	}
	copy(result[:], decoded)
	return result, nil
}

type verifiedReader struct {
	body           io.ReadCloser
	expectedSize   int64
	expectedDigest [sha256.Size]byte
	digest         hash.Hash
	count          int64
	mu             sync.Mutex
	closed         bool
	verified       bool
}

func (r *verifiedReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := r.body.Read(p)
	if n > 0 {
		r.count += int64(n)
		_, _ = r.digest.Write(p[:n])
		if r.count > r.expectedSize {
			_ = r.closeLocked()
			return n, integrityError("sandbox: download exceeds Content-Length", nil)
		}
	}
	if err == io.EOF {
		actual := r.digest.Sum(nil)
		if r.count != r.expectedSize || subtle.ConstantTimeCompare(actual, r.expectedDigest[:]) != 1 {
			_ = r.closeLocked()
			return n, integrityError("sandbox: download integrity verification failed", nil)
		}
		r.verified = true
		_ = r.closeLocked()
	}
	return n, err
}
func (r *verifiedReader) Close() error { r.mu.Lock(); defer r.mu.Unlock(); return r.closeLocked() }
func (r *verifiedReader) closeLocked() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.body.Close()
}

func (d Download) String() string { return fmt.Sprintf("Download(%d bytes)", d.SizeBytes) }
