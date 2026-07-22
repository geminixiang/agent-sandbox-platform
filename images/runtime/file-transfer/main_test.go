package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withWorkspace(t *testing.T) string {
	t.Helper()
	original := workspaceRoot
	workspaceRoot = t.TempDir()
	t.Cleanup(func() { workspaceRoot = original })
	return workspaceRoot
}

func TestDownloadSnapshotsRegularFileBeforeSuccessMarker(t *testing.T) {
	root := withWorkspace(t)
	content := []byte("stable snapshot")
	path := filepath.Join(root, "nested", "file.bin")
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := download(path, &output); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	wantPrefix := "ASP1 OK 15 " + bytesToHex(digest[:]) + "\n"
	if !bytes.Equal(output.Bytes(), append([]byte(wantPrefix), content...)) {
		t.Fatalf("output = %q", output.Bytes())
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".asp-download-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("snapshot cleanup = %v, %v", matches, err)
	}
}

func TestDownloadUnlinksSnapshotBeforeStreamingBody(t *testing.T) {
	root := withWorkspace(t)
	path := filepath.Join(root, "file.bin")
	if err := os.WriteFile(path, []byte("stream body"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	started := make(chan struct{})
	destination := &blockingWriter{started: started, blocked: blocked}
	done := make(chan error, 1)
	go func() { done <- download(path, destination) }()
	<-started
	matches, err := filepath.Glob(filepath.Join(root, ".asp-download-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("visible snapshot during streaming = %v, %v", matches, err)
	}
	close(blocked)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type blockingWriter struct {
	started chan struct{}
	blocked chan struct{}
	once    bool
}

func (w *blockingWriter) Write(value []byte) (int, error) {
	if !w.once && !bytes.HasPrefix(value, []byte("ASP1 OK ")) {
		w.once = true
		close(w.started)
		<-w.blocked
	}
	return len(value), nil
}

func TestUploadValidatesBeforeAtomicReplacement(t *testing.T) {
	root := withWorkspace(t)
	path := filepath.Join(root, "nested", "file.bin")
	if _, _, err := openParent(path, true); errorCode(err) != "INVALID_PATH" {
		t.Fatalf("missing upload parent error = %v", err)
	}
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := []byte("new content")
	wrong := sha256.Sum256([]byte("wrong"))
	if err := upload(path, int64(len(content)), wrong[:], bytes.NewReader(content), &bytes.Buffer{}); errorCode(err) != "CONTENT_DIGEST_MISMATCH" {
		t.Fatalf("digest error = %v", err)
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != "old" {
		t.Fatalf("destination changed before validation: %q", unchanged)
	}

	digest := sha256.Sum256(content)
	var response bytes.Buffer
	if err := upload(path, int64(len(content)), digest[:], bytes.NewReader(content), &response); err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(path)
	if !bytes.Equal(actual, content) || response.String() != "ASP1 OK\n" {
		t.Fatalf("content = %q, response = %q", actual, response.String())
	}
}

func TestProtocolRejectsShortLongMissingSymlinkAndNonRegularPayloads(t *testing.T) {
	root := withWorkspace(t)
	path := filepath.Join(root, "file")
	digest := sha256.Sum256([]byte("abc"))
	for name, input := range map[string][]byte{"short": []byte("ab"), "long": []byte("abcd")} {
		t.Run(name, func(t *testing.T) {
			if err := upload(path, 3, digest[:], bytes.NewReader(input), &bytes.Buffer{}); errorCode(err) != "CONTENT_LENGTH_MISMATCH" {
				t.Fatalf("length error = %v", err)
			}
		})
	}
	if err := download(path, &bytes.Buffer{}); errorCode(err) != "FILE_NOT_FOUND" {
		t.Fatalf("missing error = %v", err)
	}
	if err := download(filepath.Join(root, "missing", "file"), &bytes.Buffer{}); errorCode(err) != "FILE_NOT_FOUND" {
		t.Fatalf("missing parent error = %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := download(path, &bytes.Buffer{}); errorCode(err) != "INVALID_PATH" {
		t.Fatalf("symlink error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := download(path, &bytes.Buffer{}); errorCode(err) != "INVALID_PATH" {
		t.Fatalf("directory error = %v", err)
	}
}

func TestPathTraversalAndSymlinkedParentAreRejected(t *testing.T) {
	root := withWorkspace(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "..", "escape"), filepath.Join(root, "link", "file"), root} {
		if _, _, err := openParent(path, true); errorCode(err) != "INVALID_PATH" {
			t.Fatalf("path %q error = %v", path, err)
		}
	}
}

func errorCode(err error) string {
	var typed protocolError
	if errors.As(err, &typed) {
		return string(typed)
	}
	return ""
}

func bytesToHex(value []byte) string {
	const digits = "0123456789abcdef"
	var result strings.Builder
	result.Grow(len(value) * 2)
	for _, item := range value {
		result.WriteByte(digits[item>>4])
		result.WriteByte(digits[item&15])
	}
	return result.String()
}
