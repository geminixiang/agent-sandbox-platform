// agent-sandbox-transfer is the sandbox-side half of the ASP1 file-transfer
// protocol. It is intentionally small, dependency-free, and invoked directly
// by the control plane rather than through a shell.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const maxBytes = 64 * 1024 * 1024

var workspaceRoot = "/workspace"

type protocolError string

type postflightError struct{ error }

func (e protocolError) Error() string { return string(e) }

func main() {
	if err := run(os.Args[1:]); err != nil {
		var postflight postflightError
		if errors.As(err, &postflight) {
			os.Exit(1)
		}
		code := "TRANSFER_FAILED"
		var typed protocolError
		if errors.As(err, &typed) {
			code = string(typed)
		}
		fmt.Printf("ASP1 ERR %s\n", code)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return protocolError("INVALID_PATH")
	}
	switch args[0] {
	case "download":
		if len(args) != 2 {
			return protocolError("INVALID_PATH")
		}
		return download(args[1], os.Stdout)
	case "upload":
		if len(args) != 4 {
			return protocolError("INVALID_PATH")
		}
		size, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil || size < 0 || size > maxBytes {
			return protocolError("TRANSFER_TOO_LARGE")
		}
		digest, err := hex.DecodeString(args[3])
		if err != nil || len(digest) != sha256.Size {
			return protocolError("CONTENT_DIGEST_MISMATCH")
		}
		return upload(args[1], size, digest, os.Stdin, os.Stdout)
	default:
		return protocolError("INVALID_PATH")
	}
}

func download(path string, destination io.Writer) error {
	parent, base, err := openParent(path, false)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	sourceFD, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return protocolError("FILE_NOT_FOUND")
	}
	if err != nil {
		return protocolError("INVALID_PATH")
	}
	source := os.NewFile(uintptr(sourceFD), "source")
	if source == nil {
		_ = unix.Close(sourceFD)
		return protocolError("TRANSFER_FAILED")
	}
	defer source.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(sourceFD, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return protocolError("INVALID_PATH")
	}
	if stat.Size > maxBytes {
		return protocolError("TRANSFER_TOO_LARGE")
	}

	temporaryFD, temporaryName, err := createTemporary(parent, ".asp-download-")
	if err != nil {
		return protocolError("TRANSFER_FAILED")
	}
	temporary := os.NewFile(uintptr(temporaryFD), "snapshot")
	if temporary == nil {
		_ = unix.Close(temporaryFD)
		_ = unix.Unlinkat(parent, temporaryName, 0)
		return protocolError("TRANSFER_FAILED")
	}
	defer func() {
		_ = temporary.Close()
		if temporaryName != "" {
			_ = unix.Unlinkat(parent, temporaryName, 0)
		}
	}()
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, digest), io.LimitReader(source, maxBytes+1))
	if err != nil {
		return protocolError("TRANSFER_FAILED")
	}
	if written > maxBytes {
		return protocolError("TRANSFER_TOO_LARGE")
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return protocolError("TRANSFER_FAILED")
	}
	if err := unix.Unlinkat(parent, temporaryName, 0); err != nil {
		return protocolError("TRANSFER_FAILED")
	}
	temporaryName = ""
	if _, err := fmt.Fprintf(destination, "ASP1 OK %d %x\n", written, digest.Sum(nil)); err != nil {
		return postflightError{err}
	}
	if _, err := io.CopyN(destination, temporary, written); err != nil {
		return postflightError{err}
	}
	return nil
}

func upload(path string, size int64, expected []byte, source io.Reader, response io.Writer) error {
	parent, base, err := openParent(path, true)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	if existing, openErr := unix.Openat(parent, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); openErr == nil {
		var stat unix.Stat_t
		statErr := unix.Fstat(existing, &stat)
		unix.Close(existing)
		if statErr != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG {
			return protocolError("INVALID_PATH")
		}
	} else if !errors.Is(openErr, unix.ENOENT) {
		return protocolError("INVALID_PATH")
	}

	temporary, temporaryName, err := createTemporary(parent, ".asp-upload-")
	if err != nil {
		return protocolError("TRANSFER_FAILED")
	}
	committed := false
	defer func() {
		unix.Close(temporary)
		if !committed {
			_ = unix.Unlinkat(parent, temporaryName, 0)
		}
	}()
	output := os.NewFile(uintptr(temporary), "upload")
	digest := sha256.New()
	written, copyErr := io.CopyN(io.MultiWriter(output, digest), source, size)
	if copyErr != nil || written != size {
		return protocolError("CONTENT_LENGTH_MISMATCH")
	}
	var extra [1]byte
	if count, readErr := source.Read(extra[:]); count != 0 || (readErr != nil && !errors.Is(readErr, io.EOF)) {
		return protocolError("CONTENT_LENGTH_MISMATCH")
	}
	if !equalDigest(digest.Sum(nil), expected) {
		return protocolError("CONTENT_DIGEST_MISMATCH")
	}
	if err := output.Sync(); err != nil {
		return protocolError("TRANSFER_FAILED")
	}
	if err := output.Close(); err != nil {
		return protocolError("TRANSFER_FAILED")
	}
	temporary = -1
	if err := unix.Renameat(parent, temporaryName, parent, base); err != nil {
		return protocolError("TRANSFER_FAILED")
	}
	committed = true
	if _, err := fmt.Fprint(response, "ASP1 OK\n"); err != nil {
		return postflightError{err}
	}
	return nil
}

func openParent(path string, upload bool) (int, string, error) {
	if !strings.HasPrefix(path, workspaceRoot+"/") {
		return -1, "", protocolError("INVALID_PATH")
	}
	relative := strings.TrimPrefix(path, workspaceRoot+"/")
	parts := strings.Split(relative, "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return -1, "", protocolError("INVALID_PATH")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return -1, "", protocolError("INVALID_PATH")
		}
	}
	current, err := unix.Open(workspaceRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", protocolError("INVALID_PATH")
	}
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(current, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(current)
		if errors.Is(openErr, unix.ENOENT) {
			if upload {
				return -1, "", protocolError("INVALID_PATH")
			}
			return -1, "", protocolError("FILE_NOT_FOUND")
		}
		if openErr != nil {
			return -1, "", protocolError("INVALID_PATH")
		}
		current = next
	}
	return current, parts[len(parts)-1], nil
}

func createTemporary(parent int, prefix string) (int, string, error) {
	for attempts := 0; attempts < 16; attempts++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return -1, "", err
		}
		name := prefix + hex.EncodeToString(random[:])
		fd, err := unix.Openat(parent, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		return fd, name, err
	}
	return -1, "", errors.New("temporary file collision")
}

func equalDigest(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for index := range left {
		different |= left[index] ^ right[index]
	}
	return different == 0
}
