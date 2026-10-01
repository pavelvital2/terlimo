package servicechannel

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"wg-turn-client/wlwire"
)

const frameDiagWindow = 120 * time.Second
const frameDiagLimit = 512

// FrameCapture is an opt-in, service-only observation sink. No payload is accepted.
// Pump conn/record ordinals are NOT a mapping from encrypted records to wire IDs.
type FrameCapture struct {
	mu          sync.Mutex
	started     time.Time
	lines       int
	closed      bool
	connections uint64
	out         io.Writer
	timer       *time.Timer
}

func NewFrameCapture(enabled bool, out io.Writer) *FrameCapture {
	if !enabled {
		return nil
	}
	c := &FrameCapture{started: time.Now(), out: out}
	c.mu.Lock()
	c.timer = time.AfterFunc(frameDiagWindow, func() { c.finish("WINDOW_END", true) })
	c.mu.Unlock()
	return c
}

func (c *FrameCapture) finish(reason string, truncated bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finishLocked(reason, truncated)
}
func (c *FrameCapture) finishLocked(reason string, truncated bool) {
	if c.closed {
		return
	}
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
	}
	fmt.Fprintf(c.out, "svcframe: final=true truncated=%t reason=%s elapsed_ms=%d lines=%d\n", truncated, reason, time.Since(c.started).Milliseconds(), c.lines)
}
func (c *FrameCapture) Close() { c.finish("CAPTURE_CLOSE", false) }

func (c *FrameCapture) emit(phase string, gen, xid, conn, record uint64, id *wlwire.ID, offset, size, written int, result, reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	elapsed := time.Since(c.started)
	if elapsed >= frameDiagWindow {
		c.finishLocked("WINDOW_END", true)
		return
	}
	if c.lines >= frameDiagLimit {
		c.finishLocked("LINE_LIMIT", true)
		return
	}
	c.lines++
	wireID := "-"
	if id != nil {
		wireID = RequestID(*id)
	}
	fmt.Fprintf(c.out, "svcframe: phase=%s gen=%d xid=%d conn=%d record=%d wire_id=%s offset=%d bytes=%d written=%d result=%s reason=%s elapsed_ms=%d\n", phase, gen, xid, conn, record, wireID, offset, size, written, result, reason, elapsed.Milliseconds())
}

func (c *FrameCapture) frame(gen, xid uint64, id wlwire.ID, offset, size, written int, err error) {
	if c == nil {
		return
	}
	c.emit("FRAME_WRITE", gen, xid, 0, 0, &id, offset, size, written, traceErrClass(context.Background(), err), "NONE")
}

type frameCaptureKey struct{}

func WithFrameCapture(ctx context.Context, capture *FrameCapture) context.Context {
	if capture == nil {
		return ctx
	}
	return context.WithValue(ctx, frameCaptureKey{}, capture)
}

// PumpDiag is owned by one transport's outbound pump; capture serializes emission.
type PumpDiag struct {
	capture *FrameCapture
	conn    uint64
}

func NewPumpDiag(ctx context.Context) *PumpDiag {
	c, _ := ctx.Value(frameCaptureKey{}).(*FrameCapture)
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.connections++
	conn := c.connections
	c.mu.Unlock()
	return &PumpDiag{c, conn}
}
func (d *PumpDiag) Record(phase string, record uint64, size, written int, err error) {
	if d == nil {
		return
	}
	switch phase {
	case "PUMP_DEQUEUE", "WRAP", "TURN_WRITE":
	default:
		return
	}
	d.capture.emit(phase, 0, 0, d.conn, record, nil, 0, size, written, traceErrClass(context.Background(), err), "NONE")
}
func (d *PumpDiag) Close(reason string, err error) {
	if d == nil {
		return
	}
	switch reason {
	case "RESOURCE_CLOSE", "RELAY_READ", "PIPE_WRITE", "PIPE_READ", "WRAP", "RELAY_WRITE":
	default:
		return
	}
	d.capture.emit("CONNECTION_CLOSE", 0, 0, d.conn, 0, nil, 0, 0, 0, traceErrClass(context.Background(), err), reason)
}
