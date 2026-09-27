package main

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	echoProbeBudget  = 5 * time.Second
	echoProbeRecords = 4
)

// errEchoUnconfirmed reports a connection that delivered only bounded non-echo
// records and never the keepalive marker.
var errEchoUnconfirmed = errors.New("RTT_ECHO_UNCONFIRMED")

// measureEchoRTT measures the DTLS-level keepalive echo round trip on one
// already-established connection.
//
// Timing boundary: t0 is taken immediately before a single write of the
// one-byte keepalive marker 0xFF, and the returned duration is the time until
// the matching one-byte 0xFF pong is read back on the SAME connection.
// Credential fetch and TURN/WRAP/DTLS transport setup happen strictly before
// this window and are never included in the returned duration. At most
// echoProbeRecords read calls are consumed; other records are ignored but the
// loop stays bounded. The deadline is the earlier of the caller's ctx deadline
// and echoProbeBudget. Every error path returns a zero duration, never a
// partial RTT.
func measureEchoRTT(ctx context.Context, conn net.Conn) (time.Duration, error) {
	if conn == nil {
		return 0, errors.New("TRANSPORT_FAILED")
	}
	budget := echoProbeBudget
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < budget {
			budget = remaining
		}
	}
	if budget <= 0 {
		return 0, context.DeadlineExceeded
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := conn.SetDeadline(time.Now().Add(budget)); err != nil {
		return 0, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	t0 := time.Now()
	if _, err := conn.Write([]byte{keepaliveByte}); err != nil {
		return 0, echoProbeError(ctx, err)
	}
	buffer := make([]byte, readBufSize)
	for record := 0; record < echoProbeRecords; record++ {
		n, err := conn.Read(buffer)
		if err != nil {
			return 0, echoProbeError(ctx, err)
		}
		if n == 1 && buffer[0] == keepaliveByte {
			return time.Since(t0), nil
		}
	}
	return 0, errEchoUnconfirmed
}

// echoProbeError attributes a read/write failure to context cancellation first
// and to the bounded echo deadline second.
func echoProbeError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	return err
}

// ─── connected echo probe (S5 07.2) ─────────────────────────────────────────
//
// The pre-connect measureEchoRTT above owns a freshly dialed connection. The
// types below instead share the already-Connected DTLS session with its
// periodic keepalive and wake-probe writers: one single-byte 0xFF echo is
// correlated by sequence numbers, so a manual Connected probe needs no second
// dial, no TURN allocation and no change to the user data path.

var (
	// errEchoProbeBusy reports a second connected probe while one is pending.
	errEchoProbeBusy = errors.New("RTT_PROBE_BUSY")
	// errEchoProbeUnavailable reports a connected probe without a registered
	// live config session.
	errEchoProbeUnavailable = errors.New("RTT_PROBE_UNAVAILABLE")
	// errEchoSessionStale reports an echo result produced by a session that was
	// replaced while the measurement was in flight.
	errEchoSessionStale = errors.New("RTT_SESSION_STALE")
	// errEchoChannelDirty reports a live marker channel with at least one
	// written ping still unanswered. A cancelled or timed-out probe leaves its
	// ping outstanding, so an old in-flight pong could be read as the answer to
	// a fresh ping. The live echo fails closed instead of risking that false
	// settle; the caller may fall back to a fresh direct connection.
	errEchoChannelDirty = errors.New("RTT_ECHO_CHANNEL_DIRTY")
)

// connectedEchoGeneration stamps every connected session so a result produced
// by a replaced session can never be delivered as a fresh one.
var connectedEchoGeneration atomic.Uint64

// echoProbeResult is one settled waiter outcome. A canceled or timed-out
// waiter always carries a zero duration.
type echoProbeResult struct {
	rtt time.Duration
	err error
}

// rttEchoWaiter is the single pending connected echo waiter. It is settled at
// most once, either by the matching pong or by cancel/timeout/session close.
type rttEchoWaiter struct {
	expectSeq uint64
	started   time.Time
	mu        sync.Mutex
	settled   bool
	result    chan echoProbeResult
}

// deliver settles the waiter with the elapsed time when seq is exactly the
// sequence captured at arm time.
func (w *rttEchoWaiter) deliver(seq uint64) bool {
	if seq != w.expectSeq {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.settled {
		return true
	}
	w.settled = true
	w.result <- echoProbeResult{rtt: time.Since(w.started)}
	return true
}

// cancel settles the waiter with a zero duration and the reason. It reports
// false when the waiter was already settled by its matching pong, which keeps
// the measured duration.
func (w *rttEchoWaiter) cancel(reason error) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.settled {
		return false
	}
	w.settled = true
	w.result <- echoProbeResult{err: reason}
	return true
}

func (w *rttEchoWaiter) pending() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.settled
}

// rttEchoSequencer correlates the single-byte 0xFF keepalive markers written on
// one live session with the 0xFF pongs read back on that same session.
//
// Semantics:
//   - pingSeq counts every 0xFF write (periodic keepalive, wake probe,
//     connected probe). Every claim happens under the session write lock and
//     the claiming writer holds that lock until its byte is written, so the
//     claim order is the wire order and a probe's expected sequence is always
//     the sequence of its own write. While a waiter is armed the non-probe
//     writers are suppressed, so pingSeq advances only for the probe until it
//     settles.
//   - pongSeq counts the pongs that answer the written pings. A pong that
//     would push pongSeq beyond pingSeq is unsolicited/extra: it is ignored
//     for RTT (the read loop still counts it as liveness) and cannot shift the
//     sequence for later probes.
//   - a pong is delivered only when pongSeq == the armed waiter's expectSeq.
//     Pongs below expectSeq are stale and never settle the waiter.
//   - cancel/timeout never rebases pongSeq onto pingSeq: the unanswered ping
//     stays outstanding and the channel remains dirty (pingSeq != pongSeq)
//     until its answer arrives, so a fresh RequestRTT is refused rather than
//     risking that the old pong settles it.
//   - at most one waiter exists; a second arm returns errEchoProbeBusy.
type rttEchoSequencer struct {
	mu      sync.Mutex
	pingSeq uint64
	pongSeq uint64
	waiter  *rttEchoWaiter
}

// claimMarkerWrite claims the next ping sequence under the sequencer mutex. It
// returns false while a probe waiter is armed, so the periodic keepalive and
// the wake retries keep their timeout semantics without stealing the probe's
// pong.
//
// Callers must already hold the session write lock: the claim and the marker
// byte that follows it must be one atomic step with respect to RequestRTT,
// which claims, arms and writes under that same lock. Claiming before taking
// the lock would let a marker keep an already-claimed lower sequence while the
// probe claims, arms and writes its own marker first.
func (q *rttEchoSequencer) claimMarkerWrite() (uint64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.waiter != nil {
		return 0, false
	}
	q.pingSeq++
	return q.pingSeq, true
}

// abortMarkerWrite rolls a claimed sequence back when no marker byte reached
// the wire.
func (q *rttEchoSequencer) abortMarkerWrite(seq uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pingSeq == seq {
		q.pingSeq--
	}
}

// armAt installs the single pending waiter for exactly the sequence just
// claimed by claimMarkerWrite. The caller holds the session write lock and
// writes the probe marker immediately after, so the probe's own write is
// exactly the armed sequence.
func (q *rttEchoSequencer) armAt(seq uint64, t0 time.Time) (*rttEchoWaiter, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.waiter != nil {
		return nil, errEchoProbeBusy
	}
	w := &rttEchoWaiter{expectSeq: seq, started: t0, result: make(chan echoProbeResult, 1)}
	q.waiter = w
	return w, nil
}

// notePong counts one received 0xFF pong and settles the armed waiter when it
// is the exact expected echo response.
func (q *rttEchoSequencer) notePong() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pongSeq++
	if q.pongSeq > q.pingSeq {
		// Unsolicited/extra pong: every written ping is already answered. It is
		// ignored for RTT and must not shift the correlation sequence for later
		// probes; the read loop still records it as liveness.
		q.pongSeq = q.pingSeq
		return
	}
	w := q.waiter
	seq := q.pongSeq
	if w == nil || seq != w.expectSeq {
		return
	}
	q.waiter = nil
	w.deliver(seq)
}

// cancelProbe disarms the given waiter and settles it with reason. A waiter
// that was still pending (cancel/timeout, not a matching pong) leaves its ping
// outstanding: its answer is unknown and may still be in flight, so the
// sequence counters are NOT rebased. The channel stays dirty until every
// written ping is answered; RequestRTT refuses a dirty channel instead of
// letting an old pong settle a fresh probe.
func (q *rttEchoSequencer) cancelProbe(w *rttEchoWaiter, reason error) {
	if w == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.waiter == w {
		q.waiter = nil
	}
	w.cancel(reason)
}

// channelClean reports whether every written ping has been answered. The live
// echo path is admitted only on a clean channel: otherwise the next pong read
// may be the answer to an older ping and would falsely settle a fresh probe.
func (q *rttEchoSequencer) channelClean() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pingSeq == q.pongSeq
}

// probePending reports whether a waiter is currently armed (one probe in
// flight). It is checked before channelClean so a second concurrent probe is
// answered busy, not dirty: an in-flight probe has its own ping outstanding.
func (q *rttEchoSequencer) probePending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.waiter != nil
}

// cancelArmed disarms and settles whatever waiter is pending. It is used on
// session close so teardown never leaves an armed waiter behind.
func (q *rttEchoSequencer) cancelArmed(reason error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	w := q.waiter
	q.waiter = nil
	if w != nil {
		w.cancel(reason)
	}
}

// connectedEcho is the per-connected-session RTT request path. It owns the
// marker sequence state, the single pending waiter and a session generation id
// used to fence results after a session restart.
type connectedEcho struct {
	generation  uint64
	conn        net.Conn
	dtlsWriteMu *sync.Mutex
	seq         rttEchoSequencer
	closed      chan struct{}
	closeOnce   sync.Once
}

// newConnectedEcho creates the request path for one live DTLS session. Every
// session gets a strictly increasing generation.
func newConnectedEcho(conn net.Conn, dtlsWriteMu *sync.Mutex) *connectedEcho {
	return &connectedEcho{
		generation:  connectedEchoGeneration.Add(1),
		conn:        conn,
		dtlsWriteMu: dtlsWriteMu,
		closed:      make(chan struct{}),
	}
}

func (e *connectedEcho) claimMarkerWrite() (uint64, bool) { return e.seq.claimMarkerWrite() }
func (e *connectedEcho) abortMarkerWrite(seq uint64)      { e.seq.abortMarkerWrite(seq) }
func (e *connectedEcho) notePong()                        { e.seq.notePong() }
func (e *connectedEcho) channelClean() bool               { return e.seq.channelClean() }

// RequestRTT writes one 0xFF marker on the live session and returns the time
// until the matching 0xFF pong is read back on that same session. The window
// is the earlier of the caller ctx deadline and echoProbeBudget=5s; cancel,
// timeout, write failure and session close all return a zero duration.
//
// It answers errEchoProbeBusy while another waiter is armed (at most one probe
// in flight) and, once idle, fails closed with errEchoChannelDirty while any
// earlier ping is still unanswered: on a dirty channel the next pong may be an
// old answer and cannot be attributed to a fresh marker.
func (e *connectedEcho) RequestRTT(ctx context.Context) (time.Duration, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	budget := echoProbeBudget
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < budget {
			budget = remaining
		}
	}
	if budget <= 0 {
		return 0, context.DeadlineExceeded
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	select {
	case <-e.closed:
		return 0, context.Canceled
	default:
	}
	t0 := time.Now()
	// Hold the session write lock across claim+arm+write: the probe claims its
	// sequence first and arms the waiter with exactly that sequence BEFORE the
	// write, and no marker claim (and therefore no marker write) can interleave
	// between the claim and the probe's own write. A second probe and every
	// non-probe marker writer see the armed waiter and fail/bow out.
	e.dtlsWriteMu.Lock()
	if e.seq.probePending() {
		e.dtlsWriteMu.Unlock()
		return 0, errEchoProbeBusy
	}
	if !e.seq.channelClean() {
		e.dtlsWriteMu.Unlock()
		return 0, errEchoChannelDirty
	}
	seq, ok := e.seq.claimMarkerWrite()
	if !ok {
		e.dtlsWriteMu.Unlock()
		return 0, errEchoProbeBusy
	}
	w, err := e.seq.armAt(seq, t0)
	if err != nil {
		e.seq.abortMarkerWrite(seq)
		e.dtlsWriteMu.Unlock()
		return 0, err
	}
	defer e.seq.cancelProbe(w, context.Canceled)
	_ = e.conn.SetWriteDeadline(t0.Add(budget))
	n, writeErr := e.conn.Write([]byte{keepaliveByte})
	e.dtlsWriteMu.Unlock()
	if writeErr != nil || n != 1 {
		e.seq.abortMarkerWrite(seq)
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		e.seq.cancelProbe(w, writeErr)
		return 0, echoProbeError(ctx, writeErr)
	}
	return e.wait(ctx, w, budget)
}

func (e *connectedEcho) wait(ctx context.Context, w *rttEchoWaiter, budget time.Duration) (time.Duration, error) {
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case result := <-w.result:
		return result.rtt, result.err
	case <-ctx.Done():
		e.seq.cancelProbe(w, ctx.Err())
		return 0, ctx.Err()
	case <-timer.C:
		e.seq.cancelProbe(w, context.DeadlineExceeded)
		return 0, context.DeadlineExceeded
	case <-e.closed:
		e.seq.cancelProbe(w, context.Canceled)
		return 0, context.Canceled
	}
}

// close ends the session request path: no new probe can arm and any pending
// waiter is settled. It is idempotent and performs no I/O.
func (e *connectedEcho) close() {
	e.closeOnce.Do(func() {
		e.seq.cancelArmed(context.Canceled)
		close(e.closed)
	})
}

// writeConnectedMarker writes one 0xFF keepalive marker for a live session
// under the shared session write lock. The lock is taken BEFORE the sequence
// is claimed and is held until the marker byte reaches the wire, so a marker
// claim can never be overtaken by a RequestRTT probe that claims, arms and
// writes under that same lock. While a connected echo probe is armed the write
// is suppressed and the call reports success: the periodic keepalive and the
// wake-probe retries keep their existing timeout semantics for the bounded
// probe window (<= echoProbeBudget) instead of stealing the probe's pong.
//
// A failed or short write rolls the claim back, clears the poisoned write
// deadline and reports false; a successful marker keeps its sequence so its
// own pong is counted exactly once.
func writeConnectedMarker(conn net.Conn, dtlsWriteMu *sync.Mutex, echo *connectedEcho, now time.Time) bool {
	ping := []byte{keepaliveByte}
	dtlsWriteMu.Lock()
	defer dtlsWriteMu.Unlock()
	seq, ok := echo.claimMarkerWrite()
	if !ok {
		// A probe waiter is armed/held: suppress this marker (periodic
		// keepalive, wake retry) and report success without writing.
		return true
	}
	_ = conn.SetWriteDeadline(now.Add(echoProbeBudget))
	n, err := conn.Write(ping)
	if err != nil || n != len(ping) {
		echo.abortMarkerWrite(seq)
		_ = conn.SetWriteDeadline(time.Time{})
		return false
	}
	// Clear the bounded marker deadline after a successful write: the live data
	// path must never inherit a stale deadline from a keepalive marker.
	_ = conn.SetWriteDeadline(time.Time{})
	return true
}

// handleConnectedRecord applies the marker handling for one record read from
// the live session. It returns false when the record is the single-byte 0xFF
// keepalive marker, after feeding the echo sequencer and the existing liveness
// hook; every other record returns true with its bytes untouched, so the
// dispatcher/user-data path stays byte-for-byte identical.
func handleConnectedRecord(record []byte, echo *connectedEcho, notePong func()) bool {
	if len(record) != 1 || record[0] != keepaliveByte {
		return true
	}
	if echo != nil {
		echo.notePong()
	}
	if notePong != nil {
		notePong()
	}
	return false
}
