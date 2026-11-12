//go:build darwin

package runner

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the uid of the process on the other end of a unix socket (LOCAL_PEERCRED).
func peerUID(c net.Conn) (uint32, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var uid uint32
	got := false
	raw.Control(func(fd uintptr) {
		if x, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED); err == nil {
			uid, got = x.Uid, true
		}
	})
	return uid, got
}
