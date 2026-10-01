package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Enabled only by managedServiceEstablish, never by the VPN transport caller.
// Times are captured before locking/emitting. At return, seal the buffer without
// waiting for relay goroutines: a still-running first write has no END boundary.
const serviceDialEventCap = 64

var serviceDialCalls atomic.Uint64

type serviceDialKey struct{}
type serviceDialStage uint8

const (
	dialSocketBegin serviceDialStage = iota
	dialSocketEnd
	dialTLSBegin
	dialTLSEnd
	dialClientBegin
	dialClientEnd
	dialAllocateBegin
	dialAllocateEnd
	dialCertBegin
	dialCertEnd
	dialSemaphoreWait
	dialSemaphoreAcquired
	dialSemaphoreEnd
	dialDTLSBegin
	dialDTLSEnd
	dialFirstWriteBegin
	dialFirstWriteEnd
	dialCandidateBegin
	dialCandidateEnd
)

var serviceDialStageNames = [...]string{"SOCKET_BEGIN", "SOCKET_END", "TLS_BEGIN", "TLS_END", "CLIENT_BEGIN", "CLIENT_END", "ALLOCATE_BEGIN", "ALLOCATE_END", "CERT_BEGIN", "CERT_END", "SEMAPHORE_WAIT", "SEMAPHORE_ACQUIRED", "SEMAPHORE_END", "DTLS_BEGIN", "DTLS_END", "FIRST_WRITE_BEGIN", "FIRST_WRITE_END", "CANDIDATE_BEGIN", "CANDIDATE_END"}

type serviceDialEvent struct {
	elapsed   time.Duration
	candidate int
	transport turnTransport
	stage     serviceDialStage
	result    string
}
type serviceDialTrace struct {
	started   time.Time
	call      uint64
	mu        sync.Mutex
	events    [serviceDialEventCap]serviceDialEvent
	count     int
	truncated bool
	sealed    bool
	io        dialIO
}

func newServiceDialTrace() *serviceDialTrace {
	return &serviceDialTrace{started: time.Now(), call: serviceDialCalls.Add(1)}
}
func mobileServiceDialTrace(ctx context.Context) *serviceDialTrace {
	t, _ := ctx.Value(serviceDialKey{}).(*serviceDialTrace)
	return t
}
func serviceDialResult(err error) string {
	if err == nil {
		return "OK"
	}
	if errors.Is(err, context.Canceled) {
		return "CANCELED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "TIMEOUT"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "TIMEOUT"
	}
	if errors.Is(err, io.EOF) {
		return "EOF"
	}
	if errors.Is(err, net.ErrClosed) {
		return "CLOSED"
	}
	return "OTHER"
}
func serviceDialTransportName(candidate int, tr turnTransport) string {
	if candidate == 0 {
		return "NONE"
	}
	switch tr {
	case turnTransportUDP:
		return "UDP"
	case turnTransportTCP:
		return "TCP"
	case turnTransportTLS:
		return "TLS"
	}
	return "NONE"
}
func (t *serviceDialTrace) note(stage serviceDialStage, candidate int, tr turnTransport, err error, begin bool) {
	if t == nil {
		return
	}
	elapsed := time.Since(t.started) // before observer work, never an emit timestamp
	result := "BEGIN"
	if !begin {
		result = serviceDialResult(err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed {
		return
	}
	if t.count == len(t.events) {
		t.truncated = true
		return
	}
	t.events[t.count] = serviceDialEvent{elapsed, candidate, tr, stage, result}
	t.count++
}
func (t *serviceDialTrace) finish(err error, output io.Writer) {
	elapsed := time.Since(t.started)
	t.mu.Lock()
	if t.sealed {
		t.mu.Unlock()
		return
	}
	t.sealed = true
	events := make([]serviceDialEvent, 0, t.count)
	for _, ev := range t.events[:t.count] {
		// finish captured its boundary before taking the lock. A racing completion
		// after that boundary is late even if it acquired the lock before seal.
		if ev.elapsed <= elapsed {
			events = append(events, ev)
		}
	}
	truncated := t.truncated
	ioSnapshot := t.io
	t.mu.Unlock()
	// Concurrent write boundaries may acquire the mutex in a different order.
	sort.SliceStable(events, func(i, j int) bool { return events[i].elapsed < events[j].elapsed })
	var b strings.Builder
	for _, ev := range events {
		fmt.Fprintf(&b, "dialstage: call=%d candidate=%d transport=%s stage=%s result=%s elapsed_ms=%d\n", t.call, ev.candidate, serviceDialTransportName(ev.candidate, ev.transport), serviceDialStageNames[ev.stage], ev.result, ev.elapsed.Milliseconds())
	}
	writeDialIO(&b, t.call, ioSnapshot, elapsed)
	flag := 0
	if truncated {
		flag = 1
	}
	fmt.Fprintf(&b, "dialstage: call=%d candidate=0 transport=NONE stage=FINISH result=%s elapsed_ms=%d truncated=%d\n", t.call, serviceDialResult(err), elapsed.Milliseconds(), flag)
	// One write after all measured stages; output errors never change dial results.
	_, _ = io.WriteString(output, b.String())
}
func (t *serviceDialTrace) emit(err error) { t.finish(err, os.Stderr) }

type firstServiceDialWrite struct {
	net.PacketConn
	trace     *serviceDialTrace
	candidate int
	transport turnTransport
	claimed   atomic.Bool
}

func observeFirstServiceDialWrite(conn net.PacketConn, t *serviceDialTrace, candidate int, tr turnTransport) net.PacketConn {
	if t == nil {
		return conn
	}
	return &firstServiceDialWrite{PacketConn: conn, trace: t, candidate: candidate, transport: tr}
}
func (c *firstServiceDialWrite) WriteTo(data []byte, addr net.Addr) (int, error) {
	first := c.claimed.CompareAndSwap(false, true)
	if first {
		c.trace.note(dialFirstWriteBegin, c.candidate, c.transport, nil, true)
	}
	n, err := c.PacketConn.WriteTo(data, addr)
	if first {
		c.trace.note(dialFirstWriteEnd, c.candidate, c.transport, err, false)
	}
	return n, err
}
