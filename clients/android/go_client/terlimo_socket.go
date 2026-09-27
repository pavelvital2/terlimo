package main

import (
	"net"
	"sync/atomic"
	"syscall"
	"time"
)

// Only the private managed child installs this binding. The legacy client is
// unchanged. Go's raw socket syscalls bypass Android's libc process binding.
type managedSocketBinding struct{ bind func(int) error }

var managedSockets atomic.Pointer[managedSocketBinding]

func managedSocketControl(_, _ string, raw syscall.RawConn) error {
	binding := managedSockets.Load()
	if binding == nil {
		return nil
	}
	var bindErr error
	if err := raw.Control(func(fd uintptr) { bindErr = binding.bind(int(fd)) }); err != nil {
		return err
	}
	return bindErr
}

func managedNetworkDialer(timeout time.Duration) net.Dialer {
	return net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: managedSocketControl}
}
