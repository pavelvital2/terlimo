package wlbs

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// RPC uses a record-oriented net.Conn (Pion DTLS), NOT a TCP stream. Callers
// must complete certificate pin checking before constructing a usable RPC.
type RPC struct {
	Conn           net.Conn
	AttemptTimeout time.Duration
	Jitter         func(time.Duration) time.Duration
	// Optional bounded observer; no frame/body/error text is supplied to it.
	ObserveIO func(stage, operation string, err error)
	mu        sync.Mutex
}

func (r *RPC) Call(ctx context.Context, id ID, body []byte) ([]byte, error) {
	budget := MaxAttempts
	return r.CallBudget(ctx, id, body, &budget)
}

// CallBudget keeps the total business transmissions bounded across fresh proofs.
// budget is owned by the calling operation and must not be shared concurrently.
func (r *RPC) CallBudget(ctx context.Context, id ID, body []byte, budget *int) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	op := ""
	if r.ObserveIO != nil {
		var meta struct {
			Op string `json:"op"`
		}
		_ = json.Unmarshal(body, &meta)
		switch meta.Op {
		case "challenge", "catalog", "operation_status", "refresh_access", "register", "sync_access":
			op = meta.Op
		}
	}
	observe := func(stage string, err error) {
		if r.ObserveIO != nil {
			r.ObserveIO(stage, op, err)
		}
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if r.Conn == nil {
		return nil, failure("TRANSPORT_CLOSED")
	}
	frames, e := Frames(id, false, body)
	if e != nil {
		return nil, e
	}
	if budget == nil || *budget < 1 || *budget > MaxAttempts {
		return nil, failure("RETRY_EXHAUSTED")
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = r.Conn.Close()
		case <-stop:
		}
	}()
	defer func() { close(stop); <-done; _ = r.Conn.SetDeadline(time.Time{}) }()
	timeout := r.AttemptTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	var last error
	for *budget > 0 {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		*budget--
		deadline := time.Now().Add(timeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if e := r.Conn.SetDeadline(deadline); e != nil {
			return nil, e
		}
		for _, f := range frames {
			observe("WRITE", nil)
			n, err := r.Conn.Write(f)
			if err != nil {
				observe("WRITE", err)
			}
			if err != nil {
				last = err
				break
			}
			if n != len(f) {
				last = io.ErrShortWrite
				break
			}
			last = nil
		}
		if last == nil {
			assembly := NewReassembler(id, true)
			buf := make([]byte, HeaderSize+FragmentSize+1)
			for {
				if !assembly.started.IsZero() {
					d := assembly.started.Add(AssemblyTimeout)
					if d.Before(deadline) {
						_ = r.Conn.SetReadDeadline(d)
					}
				}
				observe("READ", nil)
				n, err := r.Conn.Read(buf)
				if err != nil {
					observe("READ", err)
				}
				if err != nil {
					last = err
					break
				}
				response, complete, err := assembly.Add(buf[:n], time.Now())
				if err != nil {
					return nil, err
				}
				if !complete {
					continue
				}
				var meta struct {
					V      int    `json:"v"`
					Status string `json:"status"`
					Error
				}
				if err := StrictJSON(response, &meta); err != nil {
					return nil, err
				}
				if meta.V != 1 && meta.V != 2 {
					return nil, failure("UNSUPPORTED_VERSION")
				}
				if meta.Status != "error" {
					return response, nil
				}
				if !knownError(meta.Code) || meta.RetryAfterMS < 0 {
					return nil, failure("BAD_MESSAGE")
				}
				last = &meta.Error
				break
			}
		}
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		if !retryable(last) {
			return nil, last
		}
		if *budget == 0 {
			break
		}
		delay := r.jitter(time.Second * time.Duration(1<<(MaxAttempts-*budget-1)))
		var server *Error
		if errors.As(last, &server) && server.RetryAfterMS > 0 {
			if server.RetryAfterMS > int64((24*time.Hour)/time.Millisecond) {
				return nil, failure("BAD_MESSAGE")
			}
			if wait := time.Duration(server.RetryAfterMS) * time.Millisecond; wait > delay {
				delay = wait
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if last == nil {
		last = failure("RETRY_EXHAUSTED")
	}
	var ne net.Error
	if errors.As(last, &ne) && ne.Timeout() {
		return nil, &Error{Code: "TRANSPORT_TIMEOUT", Retryable: true}
	}
	return nil, last
}

func knownError(code string) bool {
	switch code {
	case "UNSUPPORTED_VERSION", "BAD_MESSAGE", "INVALID_BOOTSTRAP", "SUBSCRIPTION_EXPIRED", "PROOF_INVALID", "DEVICE_REVOKED", "DEVICE_LIMIT_REACHED", "APPROVAL_REQUIRED", "IDEMPOTENCY_CONFLICT", "CHALLENGE_EXPIRED", "RATE_LIMITED", "BACKEND_UNAVAILABLE", "NODE_UNAVAILABLE", "OPERATION_PENDING", "OPERATION_UNKNOWN", "LEASE_CONFLICT", "GRANT_MISSING", "AUTH_REQUIRED", "GRANT_REVOKED", "LEASE_EXPIRED", "UNSUPPORTED_AUTH", "SESSION_NOT_READY":
		return true
	}
	return false
}
func (r *RPC) jitter(max time.Duration) time.Duration {
	if r.Jitter != nil {
		d := r.Jitter(max)
		if d < 0 {
			return 0
		}
		if d > max {
			return max
		}
		return d
	}
	var b [8]byte
	if _, e := rand.Read(b[:]); e != nil {
		return 0
	}
	return time.Duration(binary.BigEndian.Uint64(b[:]) % uint64(max+1))
}
func retryable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		if !e.Retryable {
			return false
		}
		switch e.Code {
		case "RATE_LIMITED", "BACKEND_UNAVAILABLE", "NODE_UNAVAILABLE", "OPERATION_PENDING", "SESSION_NOT_READY":
			return true
		}
		return false
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
