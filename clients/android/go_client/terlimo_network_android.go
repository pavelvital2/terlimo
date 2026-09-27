//go:build android && cgo

package main

/*
#include <android/multinetwork.h>
#cgo LDFLAGS: -landroid
static int bind_terlimo_network(unsigned long long handle) {
  return android_setprocnetwork((net_handle_t)handle);
}
*/
import "C"
import "errors"

// Bind only this child process: the Android host's probes stay inside its VPN.
func bindManagedNetwork(handle uint64) error {
	if handle == 0 || C.bind_terlimo_network(C.ulonglong(handle)) != 0 {
		return errors.New("PHYSICAL_NETWORK_UNAVAILABLE")
	}
	managedSockets.Store(&managedSocketBinding{bind: func(fd int) error {
		if C.android_setsocknetwork(C.net_handle_t(handle), C.int(fd)) != 0 {
			return errors.New("PHYSICAL_SOCKET_BIND_FAILED")
		}
		return nil
	}})
	return nil
}
