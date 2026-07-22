//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func childSecurityProbeMode() (bool, int) {
	if len(os.Args) != 3 || os.Args[1] != "child-security-probe" {
		return false, 0
	}
	fail := func(message string, err error) (bool, int) {
		if err == nil {
			fmt.Fprintln(os.Stderr, message)
		} else {
			fmt.Fprintln(os.Stderr, message+":", err)
		}
		return true, 1
	}
	if os.Geteuid() != 10001 || os.Getegid() != 10001 {
		return fail("unexpected child identity", nil)
	}
	groups, err := os.Getgroups()
	if err != nil || len(groups) != 0 {
		return fail("supplementary groups were not cleared", err)
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err = unix.Capget(&header, &data[0]); err != nil {
		return fail("capget", err)
	}
	for _, word := range data {
		if word.Effective != 0 || word.Permitted != 0 || word.Inheritable != 0 {
			return fail("effective, permitted, or inheritable capability remained", nil)
		}
	}
	for capability := 0; capability < 64; capability++ {
		value, ambientErr := unix.PrctlRetInt(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_IS_SET, uintptr(capability), 0, 0)
		if errors.Is(ambientErr, syscall.EINVAL) {
			break
		}
		if ambientErr != nil || value != 0 {
			return fail("ambient capability remained", ambientErr)
		}
	}
	noNewPrivileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err != nil || noNewPrivileges != 1 {
		return fail("no_new_privs is not set", err)
	}
	stateDir := os.Args[2]
	if file, openErr := os.Open(stateDir); openErr == nil {
		_ = file.Close()
		return fail("customer child opened supervisor state", nil)
	} else if !errors.Is(openErr, syscall.EACCES) {
		return fail("unexpected state access result", openErr)
	}
	connection, dialErr := net.DialTimeout("unix", filepath.Join(stateDir, "supervisor.sock"), time.Second)
	if dialErr == nil {
		_ = connection.Close()
		return fail("customer child opened supervisor socket", nil)
	}
	fmt.Print("secure")
	return true, 0
}

func capabilityProbeMode() (bool, int) {
	if len(os.Args) != 4 || os.Args[1] != "capability-probe" {
		return false, 0
	}
	pid, err := strconv.Atoi(os.Args[2])
	if err != nil || pid <= 0 {
		fmt.Fprintln(os.Stderr, "invalid target")
		return true, 2
	}
	includeKill, err := strconv.ParseBool(os.Args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid capability mode")
		return true, 2
	}
	mask := uint32(1<<unix.CAP_SETUID | 1<<unix.CAP_SETGID)
	if includeKill {
		mask |= 1 << unix.CAP_KILL
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{{Effective: mask, Permitted: mask}}
	if err = unix.Capset(&header, &data[0]); err != nil {
		fmt.Fprintln(os.Stderr, "capset:", err)
		return true, 1
	}
	err = syscall.Kill(pid, 0)
	if includeKill && err != nil {
		fmt.Fprintln(os.Stderr, "CAP_KILL did not authorize signal:", err)
		return true, 1
	}
	if !includeKill && !errors.Is(err, syscall.EPERM) {
		fmt.Fprintln(os.Stderr, "SETUID/SETGID-only result was not EPERM:", err)
		return true, 1
	}
	return true, 0
}
