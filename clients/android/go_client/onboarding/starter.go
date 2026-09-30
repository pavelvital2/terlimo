package onboarding

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"time"

	"wg-turn-client/accountaccess"
	"wg-turn-client/wlwire"
)

// BootstrapDialer opens the trusted assigned-gateway bootstrap transport for one
// explicit start. The classifier is the one-time ready-poll bootstrap secret; the
// endpoint and its pinned SPKI come from the immutable intent gateway binding, never
// from a caller guess. The returned cleanup is called exactly once after the exchange.
//
// The real implementation is the accepted DTLS/WRAP bootstrap dial
// (DialBootstrapTransport in the native runner); tests inject an in-process
// node-compatible listener.
type BootstrapDialer func(ctx context.Context, endpoint GatewayEndpoint, bootstrap Bootstrap) (net.Conn, func(), error)

// BootstrapStarter is the real Starter: it signs the exact revision-3 start RPC and
// carries it over the existing bootstrap WLBS codec (version-1 frames read verbatim
// by the node handler clientTestBootstrapServe, which is the only party that forms
// the backend envelope `{credential_id, connection_id, request_id, body}`: the node
// generates `connection_id` from its own session and `request_id` from the tunnel
// frame id; the client must not invent either field).
type BootstrapStarter struct {
	Identity Identity
	Dial     BootstrapDialer
	// Attempts bounds identical-body resends on a lost or timed-out transport
	// response; Branch B historical replay makes the exact same signed request safe.
	// Defaults to 3.
	Attempts int
	// AttemptTimeout bounds one dial+exchange attempt. Defaults to 15s.
	AttemptTimeout time.Duration
	// Observe receives secret-free stage diagnostics (stage, err) when set.
	Observe func(stage string, err error)
}

const (
	defaultStartAttempts  = 3
	defaultStartTimeout   = 15 * time.Second
	startReadFrameCeiling = 32 + wlwire.Fragment + 1
)

// Start performs the explicit signed onboarding.start RPC. It requires a ready poll
// with its one-time bootstrap credential, assigned gateway and outstanding start
// challenge; it never runs implicitly and never starts anything without a dialer.
func (s *BootstrapStarter) Start(ctx context.Context, ready IntentPoll) (StartReply, *APIError, error) {
	if s == nil || s.Dial == nil || s.Identity.Signer == nil {
		return StartReply{}, nil, ErrStartUnavailable
	}
	if ready.State != StateReady || ready.Gateway == nil || ready.Bootstrap == nil || ready.StartChallenge == nil {
		return StartReply{}, nil, ErrStartNotReady
	}
	if ready.IntentID == "" || ready.RequestKey == "" {
		return StartReply{}, nil, ErrStartNotReady
	}
	identity := s.Identity.withDefaults()
	if identity.Environment == "" || identity.InstallationID == "" {
		return StartReply{}, nil, ErrStartUnavailable
	}
	attempts := s.Attempts
	if attempts <= 0 {
		attempts = defaultStartAttempts
	}
	timeout := s.AttemptTimeout
	if timeout <= 0 {
		timeout = defaultStartTimeout
	}
	requestID, err := accountaccess.NewRequestID()
	if err != nil {
		return StartReply{}, nil, err
	}
	challenge := *ready.StartChallenge
	body, err := SignStartBody(ctx, identity, ready.IntentID, ready.RequestKey, challenge,
		challenge.NonceB64, requestID)
	if err != nil {
		return StartReply{}, nil, err
	}
	encoded, err := EncodeStartBody(body)
	if err != nil {
		return StartReply{}, nil, err
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return StartReply{}, nil, err
		}
		reply, apiError, err := s.exchange(ctx, ready, encoded, timeout)
		if err == nil {
			return reply, apiError, nil
		}
		last = err
		s.observe("attempt_failed", err)
	}
	if last == nil {
		last = errors.New("ONBOARDING_START_UNAVAILABLE")
	}
	return StartReply{}, nil, last
}

// exchangeTimeoutContext keeps the configured attempt timeout. When the operation
// carries a wait pauser, the timeout clock is controller-managed so a bounded CAPTCHA
// wait does not consume it (the duration itself is never extended).
func exchangeTimeoutContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if pauser, ok := waitPauser(ctx); ok {
		if attemptCtx, cancel, ok := pauser.TrackTimeout(timeout); ok {
			return attemptCtx, cancel
		}
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *BootstrapStarter) exchange(ctx context.Context, ready IntentPoll, body []byte,
	timeout time.Duration) (StartReply, *APIError, error) {
	attemptCtx, cancel := exchangeTimeoutContext(ctx, timeout)
	defer cancel()
	conn, cleanup, err := s.Dial(attemptCtx, ready.Gateway.Endpoint, *ready.Bootstrap)
	if err != nil {
		return StartReply{}, nil, err
	}
	defer cleanup()
	stop := context.AfterFunc(attemptCtx, func() { _ = conn.Close() })
	defer stop()

	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return StartReply{}, nil, err
	}
	var frameID wlwire.ID
	copy(frameID[:], id)
	frames, err := wlwire.Frames(frameID, false, body)
	if err != nil {
		return StartReply{}, nil, err
	}
	deadline, ok := attemptCtx.Deadline()
	if !ok {
		deadline = time.Now().Add(timeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return StartReply{}, nil, err
	}
	s.observe("send", nil)
	for _, frame := range frames {
		if _, err := conn.Write(frame); err != nil {
			s.observe("send", err)
			return StartReply{}, nil, err
		}
	}
	var assembler wlwire.Assembler
	buffer := make([]byte, startReadFrameCeiling)
	var pending []byte
	for {
		n, err := conn.Read(buffer)
		if err != nil {
			s.observe("read", err)
			return StartReply{}, nil, err
		}
		pending = append(pending, buffer[:n]...)
		for {
			frame, rest, complete := nextWireFrame(pending)
			if !complete {
				break
			}
			pending = rest
			responseID, response, err := assembler.Add(frame, time.Now())
			if err != nil {
				return StartReply{}, nil, err
			}
			if response == nil {
				continue
			}
			if responseID != frameID {
				return StartReply{}, nil, errors.New("ONBOARDING_START_REPLY_MALFORMED")
			}
			s.observe("receive", nil)
			reply, apiError, err := DecodeStartReply(response)
			if err != nil {
				return StartReply{}, nil, err
			}
			return reply, apiError, nil
		}
	}
}

// nextWireFrame splits the first complete version-1 WLBS frame off a stream
// buffer. TCP may deliver one frame in pieces or coalesce several frames into
// one read, while the assembler accepts exactly one whole frame per Add call.
// A malformed header is returned as a frame so the assembler rejects it.
func nextWireFrame(b []byte) (frame, rest []byte, complete bool) {
	if len(b) < 32 {
		return nil, b, false
	}
	total := int(binary.BigEndian.Uint32(b[24:28]))
	off := int(binary.BigEndian.Uint32(b[28:32]))
	if total < 1 || total > wlwire.MaxBody || off < 0 || off >= total || off%wlwire.Fragment != 0 {
		return b, nil, true
	}
	size := total - off
	if size > wlwire.Fragment {
		size = wlwire.Fragment
	}
	if len(b) < 32+size {
		return nil, b, false
	}
	return b[:32+size], b[32+size:], true
}

func (s *BootstrapStarter) observe(stage string, err error) {
	if s.Observe != nil {
		s.Observe(stage, err)
	}
}
