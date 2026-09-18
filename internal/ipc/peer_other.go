//go:build !linux

package ipc

import (
	"fmt"
	"net"
)

func PeerUID(conn *net.UnixConn) (int, error) {
	return -1, fmt.Errorf("daemon IPC requires Linux peer credentials")
}
