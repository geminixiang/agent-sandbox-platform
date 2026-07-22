//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

const childMarker = "__asp_supervisor_child_v1__"

func init() {
	if len(os.Args) >= 3 && os.Args[1] == childMarker {
		if err := clearChildSecurityState(); err != nil {
			_, _ = os.Stderr.WriteString("secure child trampoline: " + err.Error() + "\n")
			os.Exit(126)
		}
		if err := syscall.Exec(os.Args[2], os.Args[2:], os.Environ()); err != nil {
			_, _ = os.Stderr.WriteString("exec child: " + err.Error() + "\n")
			os.Exit(127)
		}
	}
}

func clearChildSecurityState() error {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("clear effective, permitted, and inheritable capabilities: %w", err)
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clear ambient capabilities: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	return nil
}

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
	targetPath := cmd.Path
	targetArgs := append([]string(nil), cmd.Args...)
	cmd.Path = "/proc/self/exe"
	cmd.Args = append([]string{cmd.Path, childMarker, targetPath}, targetArgs[1:]...)
	attr := &syscall.SysProcAttr{}
	containment.Configure(attr)
	if uid >= 0 && gid >= 0 {
		// os/exec fails the child before trampoline exec if setgroups/setgid/setuid fails.
		attr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}, NoSetGroups: false}
	}
	cmd.SysProcAttr = attr
}
