package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"wg-turn-client/internal/wlwire"
)

// One process-wide diagnostic window. No capture state or formatting when OFF.
var serviceFrameCapture = newServiceFrameCapture(os.Getenv("TERLIMO_SERVICE_FRAME_DIAG") == "1", time.Now())

const serviceFrameDiagWindow = 120 * time.Second
const serviceFrameDiagLines = 512

type serviceFrameCaptureState struct {
	mu      sync.Mutex
	started time.Time
	lines   int
	closed  bool
}

func newServiceFrameCapture(enabled bool, started time.Time) *serviceFrameCaptureState {
	if !enabled {
		return nil
	}
	return &serviceFrameCaptureState{started: started}
}

type serviceFrameDiag struct {
	capture   *serviceFrameCaptureState
	gen       int64
	seq       int
	reads     int
	fragments int // Successful Add calls, including identical duplicates; not unique offsets.
	wireID    wlwire.ID
	hasID     bool
	total     uint32
	offset    uint32
}

func newServiceFrameDiag(capture *serviceFrameCaptureState, gen int64, seq int) *serviceFrameDiag {
	if capture == nil {
		return nil
	}
	return &serviceFrameDiag{capture: capture, gen: gen, seq: seq}
}

func (d *serviceFrameDiag) emit(phase, result, reason string, size int) {
	if d == nil {
		return
	}
	c := d.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	elapsed, allowed := c.allowLineLocked()
	if !allowed {
		return
	}
	wireID := "-"
	if d.hasID {
		wireID = hex.EncodeToString(d.wireID[:])
	}
	log.Printf("[SVCFRAME] phase=%s gen=%d seq=%d result=%s reason=%s elapsed_ms=%d reads=%d fragments_seen=%d wire_id=%s bytes=%d total=%d offset=%d",
		phase, d.gen, d.seq, result, reason, elapsed.Milliseconds(), d.reads, d.fragments, wireID, size, d.total, d.offset)
}

// Header metadata is read only after the real codec accepted this fragment.
func (d *serviceFrameDiag) added(id wlwire.ID, frame []byte, complete bool) {
	if d == nil {
		return
	}
	d.fragments++
	d.wireID, d.hasID = id, true
	d.total = binary.BigEndian.Uint32(frame[24:28])
	d.offset = binary.BigEndian.Uint32(frame[28:32])
	result := "PARTIAL"
	if complete {
		result = "COMPLETE"
	}
	d.emit("assembler", result, "NONE", len(frame))
}

func serviceFrameDiagErr(err error) string {
	switch {
	case err == nil:
		return "OK"
	case errors.Is(err, wlwire.ErrMessage):
		return "INVALID"
	case errors.Is(err, context.Canceled):
		return "CANCELED"
	case errors.Is(err, io.EOF):
		return "EOF"
	case errors.Is(err, net.ErrClosed), errors.Is(err, dtls.ErrConnClosed):
		return "CLOSED"
	}
	// Pion's sentinel is private. Classify only its exact typed, known error;
	// raw errors (possibly containing addresses or payload) never reach logs.
	var temporary *dtls.TemporaryError
	if errors.As(err, &temporary) && temporary.Err != nil && temporary.Err.Error() == "buffer is too small" {
		return "BUFFER_TOO_SMALL"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "TIMEOUT"
	}
	return "OTHER"
}

func serviceWatcherRead(c net.Conn, diag *serviceFrameDiag) bool {
	diag.emit("watcher_begin", "WAIT", "NONE", 0)
	buf := make([]byte, 1)
	n, err := c.Read(buf)
	diag.emit("watcher_end", serviceFrameDiagErrIfOn(diag, err), "OBSERVED_READ_RESULT", n)
	return err == nil
}

// Avoid even classifying debug errors on the ordinary OFF path.
func serviceFrameDiagErrIfOn(d *serviceFrameDiag, err error) string {
	if d == nil {
		return ""
	}
	return serviceFrameDiagErr(err)
}

// Caller holds mu through the log write; every diagnostic shares this one budget.
func (c *serviceFrameCaptureState) allowLineLocked() (time.Duration, bool) {
	if c.closed {
		return 0, false
	}
	elapsed := time.Since(c.started)
	if elapsed >= serviceFrameDiagWindow || c.lines >= serviceFrameDiagLines {
		c.closed = true
		why := "LINE_LIMIT"
		if elapsed >= serviceFrameDiagWindow {
			why = "WINDOW_END"
		}
		log.Printf("[SVCFRAME] truncated=true reason=%s elapsed_ms=%d lines=%d", why, elapsed.Milliseconds(), c.lines)
		return 0, false
	}
	c.lines++
	return elapsed, true
}

// Exactly one fixed-field marker at an existing SERVICE_UNAVAILABLE return.
// No timers, classification or formatting on the nil/OFF path.
func (c *serviceFrameCaptureState) unixTerminal(ctx context.Context, requestID, stage string, err error, entered, ioStarted time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, allowed := c.allowLineLocked(); !allowed {
		return
	}
	wireID := "-"
	if serviceRequestID.MatchString(requestID) {
		wireID = requestID
	}
	ctxClass := "NONE"
	if errors.Is(ctx.Err(), context.Canceled) {
		ctxClass = "CANCELED"
	} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		ctxClass = "DEADLINE"
	}
	outcome := "OTHER"
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		outcome = "DEADLINE"
	} else if err != nil {
		outcome = serviceFrameDiagErr(err)
	}
	ioElapsed := int64(-1) // No successfully set Unix deadline yet.
	if !ioStarted.IsZero() {
		ioElapsed = time.Since(ioStarted).Milliseconds()
	}
	log.Printf("[SVCFRAME] phase=unix_terminal stage=%s outcome=%s ctx=%s elapsed_ms=%d io_elapsed_ms=%d wire_id=%s",
		stage, outcome, ctxClass, time.Since(entered).Milliseconds(), ioElapsed, wireID)
}
