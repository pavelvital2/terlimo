package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// One bounded snapshot per bootstrap synchronization. No raw errors or wire
// values are retained. Observation never closes a connection or changes errors.
type bootstrapDiagnosticKey struct{}
type bootstrapDiagnostic struct {
	mu                                       sync.Mutex
	caller, transport                        context.Context
	started                                  time.Time
	operation, stage, firstClose, errorClass string
	handshake, frozen                        bool
	message                                  bridgeMessage
}

func newBootstrapDiagnostic(ctx context.Context) *bootstrapDiagnostic {
	return &bootstrapDiagnostic{caller: ctx, started: time.Now(), operation: "UNKNOWN", stage: "DIAL", firstClose: "NONE", errorClass: "NONE"}
}
func bootstrapTrace(ctx context.Context) *bootstrapDiagnostic {
	d, _ := ctx.Value(bootstrapDiagnosticKey{}).(*bootstrapDiagnostic)
	return d
}
func (d *bootstrapDiagnostic) setTransport(ctx context.Context) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.frozen {
		d.transport = ctx
	}
}
func (d *bootstrapDiagnostic) setStage(stage string, handshake bool) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.frozen {
		d.stage = stage
		d.handshake = d.handshake || handshake
	}
}
func bootstrapErrorClass(err error) string {
	switch {
	case err == nil:
		return "NONE"
	case errors.Is(err, io.EOF):
		return "EOF"
	case errors.Is(err, context.Canceled):
		return "CANCELED"
	case errors.Is(err, context.DeadlineExceeded):
		return "DEADLINE"
	default:
		return "OTHER"
	}
}
func contextFlags(ctx context.Context) (bool, bool) {
	if ctx == nil {
		return false, false
	}
	err := ctx.Err()
	return errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded)
}
func (d *bootstrapDiagnostic) snapshotLocked(failed bool) bridgeMessage {
	cc, cd := contextFlags(d.caller)
	tc, td := contextFlags(d.transport)
	return bridgeMessage{"type": "bootstrap_diagnostic", "operation": d.operation, "io_stage": d.stage,
		"handshake_pin_ok": d.handshake, "caller_canceled": cc, "caller_deadline": cd,
		"transport_canceled": tc, "transport_deadline": td, "first_close": d.firstClose,
		"error_class": d.errorClass, "failed": failed, "elapsed_ms": time.Since(d.started).Milliseconds()}
}
func (d *bootstrapDiagnostic) observeIO(stage, op string, err error) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.frozen {
		return
	}
	d.stage = stage
	switch op {
	case "challenge":
		d.operation = "CHALLENGE"
	case "catalog":
		d.operation = "CATALOG"
	case "operation_status":
		d.operation = "STATUS"
	case "refresh_access":
		d.operation = "REFRESH"
	case "register":
		d.operation = "REGISTER"
	default:
		d.operation = "UNKNOWN"
	}
	if err != nil {
		d.errorClass = bootstrapErrorClass(err)
		if d.firstClose == "NONE" {
			if d.caller.Err() != nil {
				d.firstClose = "CALLER_CANCEL"
			} else {
				d.firstClose = "UNKNOWN"
			}
		}
		d.message = d.snapshotLocked(true)
		d.frozen = true
	}
}

// Called before existing cancellation/cleanup, never instead of it. A pump
// failure is a local close trigger, NOT proof of the remote origin of its error.
func (d *bootstrapDiagnostic) noteClose(reason string, err error) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.frozen || d.firstClose != "NONE" {
		return
	}
	if d.caller.Err() != nil {
		reason = "CALLER_CANCEL"
	}
	d.firstClose = reason
	if err != nil {
		d.errorClass = bootstrapErrorClass(err)
		d.message = d.snapshotLocked(true)
		d.frozen = true
	}
}
func (d *bootstrapDiagnostic) finish(err error) bridgeMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.message != nil {
		return d.message
	}
	d.errorClass = bootstrapErrorClass(err)
	if err != nil && d.firstClose == "NONE" {
		if d.caller.Err() != nil {
			d.firstClose = "CALLER_CANCEL"
		} else {
			d.firstClose = "UNKNOWN"
		}
	}
	if err == nil {
		d.stage = "COMPLETE"
	}
	d.message = d.snapshotLocked(err != nil)
	d.frozen = true
	return d.message
}

func (d *bootstrapDiagnostic) complete(err error, cleanup func(), emit func(bridgeMessage) error) {
	snapshot := d.finish(err)
	cleanup()
	_ = emit(snapshot)
}
