package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
)

// Process-local numbers correlate TEST transport lifetime without exporting
// access identities, network addresses, packet contents or arbitrary errors.
var managedTraceSequence atomic.Uint64

type managedConnTrace struct {
	id          uint64
	firstExit   sync.Once
	firstRead   sync.Once
	firstRecord sync.Once
}

// lengthBucket is the only length information recorded for the first record: 0, 1, 2-3 or 4+.
func lengthBucket(n int) string {
	switch {
	case n == 0:
		return "0"
	case n == 1:
		return "1"
	case n <= 3:
		return "2-3"
	default:
		return "4+"
	}
}

// firstRecordDiag logs the first post-auth record class once per connection. It records only
// the length bucket and whether the record was an exact 0xFF keepalive; never raw bytes, ids,
// addresses or packet content.
func (t *managedConnTrace) firstRecordDiag(record []byte) {
	if t == nil {
		return
	}
	t.firstRecord.Do(func() {
		log.Printf("[MANAGED_TRACE] conn=%d stage=first_record length_bucket=%s is_keepalive=%t",
			t.id, lengthBucket(len(record)), isExactKeepalive(record))
	})
}

func newManagedConnTrace() *managedConnTrace {
	return &managedConnTrace{id: managedTraceSequence.Add(1)}
}
func managedErrorClass(err error) string {
	if err == nil {
		return "none"
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, net.ErrClosed):
		return "closed"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	switch err.Error() {
	case "AUTH_REQUIRED", "PROOF_INVALID", "SESSION_NOT_READY", "LEASE_EXPIRED", "WG_GRANT_MISMATCH", "LEASE_CONFLICT":
		return err.Error()
	}
	return "other"
}
func (t *managedConnTrace) event(stage, reason string) {
	if t != nil {
		log.Printf("[MANAGED_TRACE] conn=%d stage=%s class=%s", t.id, stage, reason)
	}
}
func (t *managedConnTrace) exit(stage string, err error) {
	if t == nil {
		return
	}
	t.firstExit.Do(func() {
		class := managedErrorClass(err)
		var mismatch *wgMismatchError
		if errors.As(err, &mismatch) {
			// Bounded first-rejection line: add only the fixed classifier enums. Never
			// payload, keys, grant ids, receiver indices, addresses or exception text.
			log.Printf("[MANAGED_TRACE] conn=%d stage=%s class=%s packet_class=%s reject_reason=%s",
				t.id, stage, class, mismatch.packetClass, mismatch.reason)
			return
		}
		t.event(stage, class)
	})
}
func (t *managedConnTrace) acceptedRead() {
	if t != nil {
		t.firstRead.Do(func() { t.event("first_read_accepted", "none") })
	}
}
func traceManagedExit(conn net.Conn, stage string, err error) {
	if c, ok := conn.(*clientTestConn); ok {
		c.trace.exit(stage, err)
	}
}
func traceManagedFirstRecord(conn net.Conn, record []byte) {
	if c, ok := conn.(*clientTestConn); ok && c.trace != nil {
		c.trace.firstRecordDiag(record)
	}
}
func traceManagedEvent(conn net.Conn, stage, reason string) {
	if c, ok := conn.(*clientTestConn); ok {
		c.trace.event(stage, reason)
	}
}

// managedDenyClasses is the closed allowlist for post-read GETCONF denials. Arbitrary
// strings are collapsed to "other"; no payload, credential, registration or address is
// ever logged.
var managedDenyClasses = map[string]struct{}{
	"device_mismatch":  {},
	"deactivated":      {},
	"engine_ownership": {},
	"server_storage":   {},
	"expired":          {},
	"wrong_password":   {},
	"not_found":        {},
}

func traceManagedDeny(conn net.Conn, class string) {
	if _, ok := managedDenyClasses[class]; !ok {
		class = "other"
	}
	traceManagedEvent(conn, "denied", class)
}

func traceManagedRelay(conn net.Conn, relayID uint64) {
	if c, ok := conn.(*clientTestConn); ok && c.trace != nil {
		log.Printf("[MANAGED_TRACE] conn=%d stage=relay_attached relay=%d", c.trace.id, relayID)
	}
}
