package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"time"
)

// Ordinary error-only logging. ctxErr is a snapshot from the error exit,
// before connection defers and the caller's forced watcher unblock/cancel.
// There is no process-start window, global capture budget, or diagnostic flag.
func serviceUnixTerminal(ctxErr error, requestID, stage string, err error, entered, ioStarted time.Time) {
	ctxClass := "NONE"
	if errors.Is(ctxErr, context.Canceled) {
		ctxClass = "CANCELED"
	} else if errors.Is(ctxErr, context.DeadlineExceeded) {
		ctxClass = "DEADLINE"
	}
	outcome := "OTHER"
	// Unix-applicable bounded classes reused from 0cb8bf5 unix_terminal.
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		outcome = "DEADLINE"
	case errors.Is(err, context.Canceled):
		outcome = "CANCELED"
	case errors.Is(err, net.ErrClosed):
		outcome = "CLOSED"
	case errors.Is(err, io.EOF):
		outcome = "EOF"
	default:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			outcome = "TIMEOUT"
		}
	}
	id := "-"
	if serviceRequestID.MatchString(requestID) {
		id = requestID
	}
	ioElapsed := int64(-1)
	if !ioStarted.IsZero() {
		ioElapsed = time.Since(ioStarted).Milliseconds()
	}
	log.Printf("[SVCUNIX] terminal stage=%s outcome=%s ctx=%s elapsed_ms=%d io_elapsed_ms=%d request_id=%s", stage, outcome, ctxClass, time.Since(entered).Milliseconds(), ioElapsed, id)
}
