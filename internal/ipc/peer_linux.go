package ipc

import (
	"golang.org/x/sys/unix"
	"net"
)

func PeerUID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *unix.Ucred
	var inner error
	if err := raw.Control(func(fd uintptr) { cred, inner = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return -1, err
	}
	if inner != nil {
		return -1, inner
	}
	return int(cred.Uid), nil
}
