//go:build !linux

package runtime

import "net"

// SO_PEERCRED is Linux-only. Non-Linux builds exist for portable state/protocol tests;
// the production supervisor refuses to start outside Linux in cmd/supervisor.
func requireRootPeer(conn net.Conn) error { return nil }
