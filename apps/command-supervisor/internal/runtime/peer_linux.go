//go:build linux

package runtime

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func requireRootPeer(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("not a Unix connection")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var controlErr error
	if err = raw.Control(func(fd uintptr) { cred, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return err
	}
	if controlErr != nil {
		return controlErr
	}
	if cred == nil || cred.Uid != 0 {
		return errors.New("peer UID is not root")
	}
	return nil
}
