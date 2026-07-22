//go:build !linux

package runtime

import (
	"errors"
	"os/exec"
	"syscall"
)

type commandContainment interface {
	Configure(*syscall.SysProcAttr)
	Started(int) error
	Signal(syscall.Signal) error
}

type processGroupContainment struct{ pgid int }

func newCommandContainment() commandContainment { return &processGroupContainment{} }

func (c *processGroupContainment) Configure(attr *syscall.SysProcAttr) { attr.Setpgid = true }
func (c *processGroupContainment) Started(pid int) error {
	if pid <= 0 {
		return errors.New("invalid process group leader")
	}
	c.pgid = pid
	return nil
}
func (c *processGroupContainment) Signal(signal syscall.Signal) error {
	if c.pgid <= 0 {
		return errors.New("invalid process group")
	}
	return syscall.Kill(-c.pgid, signal)
}

func configureChild(cmd *exec.Cmd, uid, gid int, containment commandContainment) {
	attr := &syscall.SysProcAttr{}
	containment.Configure(attr)
	cmd.SysProcAttr = attr
}
