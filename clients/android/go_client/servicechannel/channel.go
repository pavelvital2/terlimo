package servicechannel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"wg-turn-client/wlwire"
)

const (
	// exchangeLimit bounds one dial+exchange attempt. It mirrors the accepted mobile
	// HTTP timeout and never extends a caller-provided deadline.
	exchangeLimit = 15 * time.Second
	readChunk     = 32 + wlwire.ServiceFragment + 1
	// The TEST gateway accepts at most eight sequential service frames per DTLS
	// connection and waits ten seconds for the next frame.
	maxSessionRequests = 8
	maxSessionIdle     = 8 * time.Second
)

var (
	// ErrTransportFailed is the fixed, secret-free transport failure code.
	ErrTransportFailed = errors.New("TRANSPORT_FAILED")
	// ErrTransportTimeout is the fixed, secret-free timeout code.
	ErrTransportTimeout = errors.New("TRANSPORT_TIMEOUT")
	// ErrBadResponse marks a peer reply that is not a valid bounded service frame.
	ErrBadResponse = errors.New("SERVICE_BAD_RESPONSE")
)

// TraceEvent is one bounded, secret-free exchange correlation record. It never carries a
// payload, URL, credential, token or raw error text: only fixed classes, counters and
// bounded durations. Exchange is a local monotonic id of this process, never any wire
// identity. Cancel vs deadline is reported through the fixed ErrClass values.
type TraceEvent struct {
	Session   uint64
	Exchange  uint64
	Class     string
	Event     string
	Reused    bool
	Requests  int
	IdleMS    int64
	ElapsedMS int64
	ErrClass  string
	Port      int
}

// Channel carries one bounded service exchange per call: exactly one establishment
// attempt, one request and one reassembled reply. There is no worker pool, no probe
// map and no catalog/grant dependency; the caller owns retries.
type Channel struct {
	// Establishment is the isolated handshake seam (see handshake.go). A nil seam
	// fails closed instead of falling back to any other transport.
	Establishment Establishment
	// Ordinary signed updates are durable for the next natural establishment;
	// the existing connection keeps its original endpoint identity until then.
	PreserveConnectionSeed bool
	// Timeout can shorten the default combined exchangeLimit. Catalog requests
	// with an explicit policy use its separate budgets instead. A shorter caller
	// deadline always wins in both modes.
	Timeout time.Duration
	// Catalog is enabled only by the mobile catalog owner. Its connection and
	// request budgets are separate; other service operations retain Timeout.
	Catalog *CatalogPolicy
	// Observe receives only fixed, secret-free stage names; it never affects I/O.
	Observe func(string)
	// Trace, when set, receives secret-free per-session correlation events. It never
	// affects I/O and never carries IP/credential/token/payload or raw error text.
	Trace         func(TraceEvent)
	mu            sync.Mutex
	conn          net.Conn
	cleanup       func()
	cancelSession context.CancelFunc
	seed          Seed
	requests      int
	lastReply     time.Time
	session       uint64
	exchange      atomic.Uint64
}

func (c *Channel) stage(name string) {
	if c != nil && c.Observe != nil {
		func() { defer func() { _ = recover() }(); c.Observe(name) }()
	}
}

// traceEvent emits one bounded, secret-free session correlation event when a Trace sink
// is set. The sink is observation-only: a panic is contained and never affects I/O.
func (c *Channel) traceEvent(ev TraceEvent) {
	if c == nil || c.Trace == nil {
		return
	}
	func() { defer func() { _ = recover() }(); c.Trace(ev) }()
}

// traceErrClass maps an exchange outcome to the fixed error-class vocabulary. The cause
// is decided by the exchange context first, so cancel vs deadline is never mixed up with
// a transport error class; no raw error text, URL or body ever leaves this function.
func traceErrClass(ctx context.Context, err error) string {
	if ctx != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "TIMEOUT"
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return "CANCELED"
		}
	}
	if err == nil {
		return "NONE"
	}
	if errors.Is(err, io.EOF) {
		return "EOF"
	}
	if errors.Is(err, net.ErrClosed) {
		return "CLOSED"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "TIMEOUT"
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "reset"):
		return "RESET"
	case strings.Contains(lower, "refused"):
		return "REFUSED"
	default:
		return "OTHER"
	}
}

type requestClassKey struct{}

// WithRequestClass tags a request context with a fixed request class (never payload).
func WithRequestClass(ctx context.Context, class string) context.Context {
	return context.WithValue(ctx, requestClassKey{}, class)
}

// RequestClass returns the fixed request class tag, defaulting to OTHER.
func RequestClass(ctx context.Context) string {
	if v, ok := ctx.Value(requestClassKey{}).(string); ok && v != "" {
		return v
	}
	return "OTHER"
}

// localUDPPort returns the local UDP source port of an established conn, or 0.
func localUDPPort(conn net.Conn) int {
	if conn == nil {
		return 0
	}
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr != nil {
		return addr.Port
	}
	return 0
}

func (c *Channel) failed(name string, ctx context.Context) {
	c.stage(name)
	if errors.Is(ctx.Err(), context.Canceled) {
		c.stage("EXCHANGE_CANCELLED")
	} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		c.stage("EXCHANGE_TIMEOUT")
	}
}

// NewChannel wires one establishment seam into a channel.
func NewChannel(establishment Establishment) *Channel {
	return &Channel{Establishment: establishment}
}

// Close ends the bounded service connection when its owning mobile attempt ends.
func (c *Channel) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
	return nil
}

func (c *Channel) closeLocked() {
	if c.conn != nil {
		c.stage("CONNECTION_CLOSED")
	}
	if c.cancelSession != nil {
		c.cancelSession()
	}
	if c.conn != nil {
		_ = c.conn.Close()
	}
	if c.cleanup != nil {
		c.cleanup()
	}
	c.conn, c.cleanup, c.cancelSession, c.requests = nil, nil, nil, 0
	c.lastReply = time.Time{}
}

// Exchange serializes at most eight requests over one bounded connection. A
// canceled or failed request closes it terminally; an idle, changed-seed or full
// connection is closed before any new request. The caller-provided id is the
// frame identity the payload's request_id must echo.
func (c *Channel) Exchange(ctx context.Context, seed Seed, id wlwire.ID, payload []byte) (body []byte, err error) {
	if len(payload) < 1 || len(payload) > wlwire.ServiceMaxFrame {
		return nil, ErrBadResponse
	}
	if c == nil {
		return nil, ErrTransportFailed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Optional service-seed reads never cause a new handshake, evict a live connection,
	// or bypass its ordinary request/idle limits.
	if optionalRequest(ctx) && (c.conn == nil || c.requests >= maxSessionRequests ||
		time.Since(c.lastReply) >= maxSessionIdle || (!c.PreserveConnectionSeed &&
		(!sameEndpoint(c.seed, seed) || c.seed.Environment != seed.Environment))) {
		return nil, ErrTransportFailed
	}
	limit := exchangeLimit
	if c != nil && c.Timeout > 0 && c.Timeout < limit {
		limit = c.Timeout
	}
	class := RequestClass(ctx)
	requestStage, requestLimit := c.Catalog.request(class)
	separate := requestStage != ""
	if separate {
		limit = c.Catalog.ConnectTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer func() { cancel() }()
	establishment := c.Establishment
	if establishment == nil {
		return nil, ErrTransportFailed
	}
	xid := c.exchange.Add(1)
	start := time.Now()
	outcomeClass := ""
	requestsAtStart := c.requests
	idleMS := int64(0)
	if !c.lastReply.IsZero() {
		idleMS = time.Since(c.lastReply).Milliseconds()
	}
	reused := false
	port := 0
	defer func() {
		cls := outcomeClass
		if cls == "" {
			cls = traceErrClass(runCtx, err)
		}
		c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "EXCHANGE_END",
			Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
			ElapsedMS: time.Since(start).Milliseconds(), ErrClass: cls, Port: port})
	}()
	if c.conn != nil {
		switch {
		case c.requests >= maxSessionRequests:
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "REUSE_STALE_REQ",
				Reused: true, Requests: c.requests, IdleMS: idleMS, Port: localUDPPort(c.conn)})
			c.closeLocked()
		case !c.PreserveConnectionSeed && (!sameEndpoint(c.seed, seed) || c.seed.Environment != seed.Environment):
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "REUSE_STALE_SEED",
				Reused: true, Requests: c.requests, IdleMS: idleMS, Port: localUDPPort(c.conn)})
			c.closeLocked()
		case idleMS >= maxSessionIdle.Milliseconds():
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "REUSE_STALE_IDLE",
				Reused: true, Requests: c.requests, IdleMS: idleMS, Port: localUDPPort(c.conn)})
			c.closeLocked()
		}
	}
	reused = c.conn != nil
	if c.conn == nil {
		if separate {
			if err := c.Catalog.emit(runCtx, "connecting_server"); err != nil {
				return nil, err
			}
		}
		// The established DTLS transport owns a session lifetime, not this HTTP
		// request context. Preserve caller values (physical network)
		// while bounding establishment by runCtx and closing at Channel.Close.
		c.stage("ESTABLISH_BEGIN")
		sessionCtx, cancelSession := context.WithCancel(context.WithoutCancel(ctx))
		stopEstablish := context.AfterFunc(runCtx, cancelSession)
		// Context completion and Establish return serialize only their observation
		// decisions. The observer runs from a small ordered queue outside the mutex,
		// so it cannot deadlock Exchange by blocking or reentering the channel.
		var observationMu sync.Mutex
		processed := false
		pendingQueued := false
		observation := make(chan string, 3)
		go func() {
			for stage := range observation {
				c.stage(stage)
			}
		}()
		stopPending := context.AfterFunc(runCtx, func() {
			observationMu.Lock()
			defer observationMu.Unlock()
			if !processed {
				if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
					observation <- "ESTABLISH_TIMEOUT_UNPROCESSED"
					pendingQueued = true
				} else if errors.Is(runCtx.Err(), context.Canceled) {
					observation <- "ESTABLISH_CANCEL_UNPROCESSED"
					pendingQueued = true
				}
			}
		})
		conn, cleanup, err := establishment.Establish(sessionCtx, seed)
		observationMu.Lock()
		processed = true
		if pendingQueued {
			// The queue preserves diagnostic order even if its observer blocks.
			observation <- "ESTABLISH_FAILED"
			if errors.Is(runCtx.Err(), context.Canceled) {
				observation <- "EXCHANGE_CANCELLED"
			} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				observation <- "EXCHANGE_TIMEOUT"
			}
		}
		close(observation)
		observationMu.Unlock()
		stopPending()
		stopEstablish()
		if err != nil {
			cancelSession()
			if !pendingQueued {
				c.failed("ESTABLISH_FAILED", runCtx)
			}
			outcomeClass = traceErrClass(runCtx, err)
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "ESTABLISH_FAIL",
				ElapsedMS: time.Since(start).Milliseconds(), ErrClass: outcomeClass})
			return nil, transportError(runCtx, err)
		}
		if conn == nil {
			cancelSession()
			if cleanup != nil {
				cleanup()
			}
			if !pendingQueued {
				c.failed("ESTABLISH_FAILED", runCtx)
			}
			outcomeClass = traceErrClass(runCtx, nil)
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "ESTABLISH_FAIL",
				ElapsedMS: time.Since(start).Milliseconds(), ErrClass: outcomeClass})
			return nil, ErrTransportFailed
		}
		if err := runCtx.Err(); err != nil {
			cancelSession()
			if cleanup != nil {
				cleanup()
			} else {
				_ = conn.Close()
			}
			if !pendingQueued {
				c.failed("ESTABLISH_FAILED", runCtx)
			}
			outcomeClass = traceErrClass(runCtx, err)
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "ESTABLISH_FAIL",
				ElapsedMS: time.Since(start).Milliseconds(), ErrClass: outcomeClass})
			return nil, err
		}
		c.conn, c.cleanup, c.cancelSession, c.seed = conn, cleanup, cancelSession, seed
		c.session++
		c.stage("ESTABLISH_OK")
		c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "ESTABLISH_OK",
			ElapsedMS: time.Since(start).Milliseconds(), Port: localUDPPort(c.conn)})
		port = localUDPPort(c.conn)
	}
	if separate {
		// Establish is finished and its cancellation bridge has been detached.
		// Ending that budget must not close the reusable transport. The caller's
		// deadline/cancellation still bounds the fresh request budget.
		cancel()
		limit = requestLimit
		runCtx, cancel = context.WithTimeout(ctx, limit)
		if err := c.Catalog.emit(runCtx, requestStage); err != nil {
			c.closeLocked()
			return nil, err
		}
	}
	// Cancellation closes the active connection immediately even while Read blocks.
	// A later request never reuses that connection.
	activeConn := c.conn
	port = localUDPPort(c.conn)
	stop := context.AfterFunc(runCtx, func() { _ = activeConn.Close() })
	defer stop()
	defer func() {
		if runCtx.Err() != nil {
			c.closeLocked()
		}
	}()
	c.stage("FRAME_WRITE_BEGIN")
	c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "WRITE_BEGIN",
		Reused: reused, Requests: requestsAtStart, IdleMS: idleMS, Port: port})
	deadline := time.Now().Add(limit)
	if ctxDeadline, ok := runCtx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		c.closeLocked()
		c.failed("FRAME_WRITE_FAILED", runCtx)
		outcomeClass = traceErrClass(runCtx, err)
		c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "WRITE_FAIL",
			Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
			ElapsedMS: time.Since(start).Milliseconds(), ErrClass: outcomeClass, Port: port})
		return nil, ErrTransportFailed
	}
	frames, err := wlwire.ServiceFrames(id, false, payload)
	if err != nil {
		c.closeLocked()
		c.failed("FRAME_WRITE_FAILED", runCtx)
		outcomeClass = traceErrClass(runCtx, err)
		c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "WRITE_FAIL",
			Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
			ElapsedMS: time.Since(start).Milliseconds(), ErrClass: outcomeClass, Port: port})
		return nil, ErrBadResponse
	}
	for _, frame := range frames {
		if _, err := c.conn.Write(frame); err != nil {
			c.closeLocked()
			c.failed("FRAME_WRITE_FAILED", runCtx)
			outcomeClass = traceErrClass(runCtx, err)
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "WRITE_FAIL",
				Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
				ElapsedMS: time.Since(start).Milliseconds(), ErrClass: outcomeClass, Port: port})
			return nil, transportError(runCtx, err)
		}
	}
	c.stage("FRAME_WRITE_OK")
	c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "WRITE_OK",
		Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
		ElapsedMS: time.Since(start).Milliseconds(), Port: port})
	c.stage("FRAME_READ_BEGIN")
	c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "READ_BEGIN",
		Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
		ElapsedMS: time.Since(start).Milliseconds(), Port: port})
	assembler := wlwire.ServiceAssembler{}
	buf := make([]byte, readChunk)
	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			c.closeLocked()
			c.failed("FRAME_READ_FAILED", runCtx)
			outcomeClass = traceErrClass(runCtx, err)
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "READ_FAIL",
				Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
				ElapsedMS: time.Since(start).Milliseconds(), ErrClass: outcomeClass, Port: port})
			return nil, transportError(runCtx, err)
		}
		replyID, body, err := assembler.Add(buf[:n], time.Now())
		if err != nil {
			c.closeLocked()
			c.failed("FRAME_INVALID", runCtx)
			outcomeClass = "OTHER"
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "READ_FAIL",
				Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
				ElapsedMS: time.Since(start).Milliseconds(), ErrClass: outcomeClass, Port: port})
			return nil, ErrBadResponse
		}
		if body == nil {
			continue
		}
		if replyID != id || len(body) == 0 {
			c.closeLocked()
			c.failed("FRAME_INVALID", runCtx)
			outcomeClass = "OTHER"
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "READ_FAIL",
				Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
				ElapsedMS: time.Since(start).Milliseconds(), ErrClass: outcomeClass, Port: port})
			return nil, ErrBadResponse
		}
		if err := runCtx.Err(); err != nil {
			c.closeLocked()
			c.failed("FRAME_READ_FAILED", runCtx)
			outcomeClass = traceErrClass(runCtx, err)
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "READ_FAIL",
				Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
				ElapsedMS: time.Since(start).Milliseconds(), ErrClass: outcomeClass, Port: port})
			return nil, err
		}
		if err := c.conn.SetDeadline(time.Time{}); err != nil {
			c.closeLocked()
			c.failed("FRAME_READ_FAILED", runCtx)
			outcomeClass = traceErrClass(runCtx, err)
			c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "READ_FAIL",
				Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
				ElapsedMS: time.Since(start).Milliseconds(), ErrClass: outcomeClass, Port: port})
			return nil, ErrTransportFailed
		}
		c.requests++
		c.lastReply = time.Now()
		c.stage("FRAME_READ_OK")
		c.traceEvent(TraceEvent{Session: c.session, Exchange: xid, Class: class, Event: "READ_OK",
			Reused: reused, Requests: requestsAtStart, IdleMS: idleMS,
			ElapsedMS: time.Since(start).Milliseconds(), Port: port})
		return body, nil
	}
}

// newServiceID returns the 16-byte frame identity the request_id echoes.
func newServiceID() (wlwire.ID, error) {
	var id wlwire.ID
	if _, err := rand.Read(id[:]); err != nil {
		return wlwire.ID{}, err
	}
	return id, nil
}

// transportError maps connection failures to fixed, secret-free codes while
// preserving the caller's cancellation. The reused main-package establishment
// reports only its own fixed codes (TRUST_FAILED, VK_API_UNAVAILABLE, TURN_EXPIRED,
// TURN_CAPACITY, BAD_MESSAGE), which pass through unchanged.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTransportTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrTransportTimeout
	}
	switch err.Error() {
	case ErrTransportTimeout.Error():
		return ErrTransportTimeout
	case ErrTransportFailed.Error():
		return ErrTransportFailed
	case "TRUST_FAILED", "VK_API_UNAVAILABLE", "TURN_EXPIRED", "TURN_CAPACITY", "BAD_MESSAGE":
		return errors.New(err.Error())
	}
	return ErrTransportFailed
}

// RequestID renders a frame identity as the contract hex32 request_id.
func RequestID(id wlwire.ID) string { return hex.EncodeToString(id[:]) }
