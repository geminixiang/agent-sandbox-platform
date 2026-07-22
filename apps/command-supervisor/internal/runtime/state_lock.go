package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// StateLock is the singleton ownership token for a supervisor state directory.
// A Manager cannot be constructed without acquiring it first.
type StateLock struct {
	mu   sync.Mutex
	file *os.File
}

func AcquireStateLock(stateDir string) (*StateLock, error) {
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	parent, base := filepath.Dir(absolute), filepath.Base(absolute)
	// The lock lives in the already-existing parent so ownership is acquired
	// before even creating or chmodding the state directory itself.
	file, err := os.OpenFile(filepath.Join(parent, "."+base+".supervisor.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another supervisor owns state directory: %w", err)
	}
	if err = file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err = os.MkdirAll(absolute, 0o700); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &StateLock{file: file}, nil
}

func (l *StateLock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
