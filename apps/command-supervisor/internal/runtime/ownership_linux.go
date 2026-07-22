//go:build linux

package runtime

import (
	"errors"
	"os"
	"syscall"
)

func ensureRootOwned(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return errors.New("supervisor state is not owned by root")
	}
	return nil
}
