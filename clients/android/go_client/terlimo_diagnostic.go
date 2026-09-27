package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Per-attempt, managed-mode-only evidence. Counts contain no packet content or
// addresses. A 92-byte datagram is length evidence, not a verified WG handshake.
type managedDiagnostics struct {
	localRXPackets, localRXBytes           atomic.Uint64
	relayRXPackets, relayRXBytes           atomic.Uint64
	relayRX92Packets                       atomic.Uint64
	localWritePackets, localWriteBytes     atomic.Uint64
	localWriteErrors, localWrite92Packets  atomic.Uint64
	configWorkerStarts, configWorkerReady  atomic.Uint64
	dataWorkerStarts, dataWorkerReady      atomic.Uint64
	dataDialOK, dataDialFailed             atomic.Uint64
	dataAuthOK, dataAuthFailed             atomic.Uint64
	dataSessionFailed, dataCanceled        atomic.Uint64
	dataTimeout, dataProofInvalid          atomic.Uint64
	dataSessionNotReady, dataLeaseConflict atomic.Uint64
	dataAuthRequired, dataOther            atomic.Uint64
	vpnState                               atomic.Uint32
	// Attempt-scoped first-cause state: every Connect creates a fresh
	// managedDiagnostics, so a late callback from an old attempt can never pollute a
	// new one. First writer wins inside the attempt.
	cancelMu     sync.Mutex
	cancelSource string
}

func (d *managedDiagnostics) noteCancelSource(source string) {
	if d == nil {
		return
	}
	d.cancelMu.Lock()
	if d.cancelSource == "" {
		d.cancelSource = source
	}
	d.cancelMu.Unlock()
}

func (d *managedDiagnostics) cancelSourceOrUnknown() string {
	if d == nil {
		return vpnCancelUnknown
	}
	d.cancelMu.Lock()
	defer d.cancelMu.Unlock()
	if d.cancelSource == "" {
		return vpnCancelUnknown
	}
	return d.cancelSource
}

type managedVPNStage uint32
type managedVPNError uint32
type managedVPNAuthCode uint32

const vpnStateFrozen uint32 = 1 << 31

// Aggregate setup termination must not replace the worker's last observation.
func (d *managedDiagnostics) freezeVPNAtSetupTimeout() {
	if d == nil {
		return
	}
	for {
		current := d.vpnState.Load()
		if current&vpnStateFrozen != 0 || d.vpnState.CompareAndSwap(current, current|vpnStateFrozen) {
			return
		}
	}
}

const (
	vpnStagePending managedVPNStage = iota
	vpnStageHandshake
	vpnStageAuth
	vpnStageConfig
	vpnStageBridge
)

const (
	vpnAuthNone managedVPNAuthCode = iota
	vpnAuthUnknown
	vpnAuthRequired
	vpnAuthBadMessage
	vpnAuthGrantRevoked
	vpnAuthLeaseConflict
	vpnAuthChallengeExpired
	vpnAuthLeaseExpired
	vpnAuthProofInvalid
	vpnAuthTrustFailed
	vpnAuthKeyUnavailable
	vpnAuthTransportClosed
	vpnAuthRetryExhausted
)

func closedVPNAuthCode(stage managedVPNStage, err error) managedVPNAuthCode {
	if stage != vpnStageAuth || err == nil {
		return vpnAuthNone
	}
	switch managedSafeError(err).Error() {
	case "AUTH_REQUIRED":
		return vpnAuthRequired
	case "BAD_MESSAGE":
		return vpnAuthBadMessage
	case "GRANT_REVOKED":
		return vpnAuthGrantRevoked
	case "LEASE_CONFLICT":
		return vpnAuthLeaseConflict
	case "CHALLENGE_EXPIRED":
		return vpnAuthChallengeExpired
	case "LEASE_EXPIRED":
		return vpnAuthLeaseExpired
	case "PROOF_INVALID":
		return vpnAuthProofInvalid
	case "TRUST_FAILED":
		return vpnAuthTrustFailed
	case "KEY_UNAVAILABLE":
		return vpnAuthKeyUnavailable
	case "TRANSPORT_CLOSED":
		return vpnAuthTransportClosed
	case "RETRY_EXHAUSTED":
		return vpnAuthRetryExhausted
	default:
		return vpnAuthUnknown
	}
}

const (
	vpnErrorNone managedVPNError = iota
	vpnErrorTimeout
	vpnErrorCanceled
	vpnErrorFailed
)

func (d *managedDiagnostics) noteVPN(stage managedVPNStage, err error) {
	if d == nil {
		return
	}
	class := vpnErrorNone
	if err != nil {
		class = vpnErrorFailed
		var timeout net.Error
		switch {
		case errors.Is(err, context.Canceled):
			class = vpnErrorCanceled
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
			class = vpnErrorTimeout
		}
	}
	next := uint32(stage)<<16 | uint32(class)<<8 | uint32(closedVPNAuthCode(stage, err))
	for {
		current := d.vpnState.Load()
		if current&vpnStateFrozen != 0 || d.vpnState.CompareAndSwap(current, next) {
			return
		}
	}
}

// A config worker can cancel the shared context after recording its concrete
// terminal. Do not replace that evidence with the resulting generic cancel.
func (d *managedDiagnostics) noteVPNCancelIfNoFailure(stage managedVPNStage, err error) {
	if d == nil {
		return
	}
	for {
		current := d.vpnState.Load()
		if current&vpnStateFrozen != 0 {
			return
		}
		if managedVPNError((current>>8)&0xff) != vpnErrorNone {
			return
		}
		next := uint32(stage)<<16 | uint32(vpnErrorCanceled)<<8
		if d.vpnState.CompareAndSwap(current, next) {
			return
		}
	}
}

func (d *managedDiagnostics) vpnEvidence() (string, string) {
	stage, failure, _ := d.vpnSnapshot()
	return stage, failure
}

func (d *managedDiagnostics) vpnSnapshot() (string, string, string) {
	stages := [...]string{"PENDING", "HANDSHAKE", "AUTH", "CONFIG", "BRIDGE"}
	errors := [...]string{"NONE", "TIMEOUT", "CANCELED", "FAILED"}
	authCodes := [...]string{"NONE", "UNKNOWN", "AUTH_REQUIRED", "BAD_MESSAGE", "GRANT_REVOKED", "LEASE_CONFLICT", "CHALLENGE_EXPIRED", "LEASE_EXPIRED", "PROOF_INVALID", "TRUST_FAILED", "KEY_UNAVAILABLE", "TRANSPORT_CLOSED", "RETRY_EXHAUSTED"}
	state := d.vpnState.Load() &^ vpnStateFrozen
	stage, failure, authCode := state>>16, (state>>8)&0xff, state&0xff
	if int(stage) >= len(stages) || int(failure) >= len(errors) || int(authCode) >= len(authCodes) {
		return "PENDING", "FAILED", "UNKNOWN"
	}
	return stages[stage], errors[failure], authCodes[authCode]
}

type managedDataStage uint8

const (
	managedDataDial managedDataStage = iota
	managedDataAuth
	managedDataSession
)

// Cumulative data-worker attempt evidence only. Cancellation is separate from
// failure; codes are fixed counters, never raw error strings or worker IDs.
func (d *managedDiagnostics) noteDataStage(config bool, stage managedDataStage, err error) {
	if d == nil || config {
		return
	}
	if errors.Is(err, context.Canceled) {
		d.dataCanceled.Add(1)
		return
	}
	switch stage {
	case managedDataDial:
		if err == nil {
			d.dataDialOK.Add(1)
		} else {
			d.dataDialFailed.Add(1)
		}
	case managedDataAuth:
		if err == nil {
			d.dataAuthOK.Add(1)
		} else {
			d.dataAuthFailed.Add(1)
		}
	case managedDataSession:
		if err != nil {
			d.dataSessionFailed.Add(1)
		}
	}
	if err == nil {
		return
	}
	switch managedSafeError(err).Error() {
	case "TRANSPORT_TIMEOUT":
		d.dataTimeout.Add(1)
	case "PROOF_INVALID":
		d.dataProofInvalid.Add(1)
	case "SESSION_NOT_READY":
		d.dataSessionNotReady.Add(1)
	case "LEASE_CONFLICT":
		d.dataLeaseConflict.Add(1)
	case "AUTH_REQUIRED":
		d.dataAuthRequired.Add(1)
	default:
		d.dataOther.Add(1)
	}
}

func (d *managedDiagnostics) noteLocalRX(n int) {
	if d != nil {
		d.localRXPackets.Add(1)
		d.localRXBytes.Add(uint64(n))
	}
}

// Relay RX excludes DTLS/control/keepalive records and is counted before the
// dispatcher queue, including packets which cannot reach a local UDP recipient.
func (d *managedDiagnostics) noteRelayRX(n int) {
	if d != nil {
		d.relayRXPackets.Add(1)
		d.relayRXBytes.Add(uint64(n))
		if n == 92 {
			d.relayRX92Packets.Add(1)
		}
	}
}

func (d *managedDiagnostics) noteLocalWrite(size, written int, err error) {
	if d == nil {
		return
	}
	if err != nil || written != size {
		d.localWriteErrors.Add(1)
		return
	}
	d.localWritePackets.Add(1)
	d.localWriteBytes.Add(uint64(written))
	if size == 92 {
		d.localWrite92Packets.Add(1)
	}
}

// Starts count validated managed session dial attempts (including retries).
// Ready counts completed dispatcher registrations, never VPN readiness.
func (d *managedDiagnostics) noteWorker(config, ready bool) {
	if d == nil {
		return
	}
	switch {
	case config && ready:
		d.configWorkerReady.Add(1)
	case config:
		d.configWorkerStarts.Add(1)
	case ready:
		d.dataWorkerReady.Add(1)
	default:
		d.dataWorkerStarts.Add(1)
	}
}

func (d *managedDiagnostics) message() bridgeMessage {
	vpnStage, vpnError, vpnAuthCode := d.vpnSnapshot()
	// Explicit numeric allowlist; do not merge transport objects or raw errors.
	return bridgeMessage{
		"type":                               "diagnostic",
		"local_udp_rx_packets":               d.localRXPackets.Load(),
		"local_udp_rx_bytes":                 d.localRXBytes.Load(),
		"relay_rx_packets":                   d.relayRXPackets.Load(),
		"relay_rx_bytes":                     d.relayRXBytes.Load(),
		"relay_rx_92_packets":                d.relayRX92Packets.Load(),
		"local_udp_write_packets":            d.localWritePackets.Load(),
		"local_udp_write_bytes":              d.localWriteBytes.Load(),
		"local_udp_write_errors":             d.localWriteErrors.Load(),
		"local_udp_write_92_packets":         d.localWrite92Packets.Load(),
		"config_worker_start_count":          d.configWorkerStarts.Load(),
		"config_worker_ready_count":          d.configWorkerReady.Load(),
		"data_worker_start_count":            d.dataWorkerStarts.Load(),
		"data_worker_ready_count":            d.dataWorkerReady.Load(),
		"data_dial_success_count":            d.dataDialOK.Load(),
		"data_dial_failure_count":            d.dataDialFailed.Load(),
		"data_auth_success_count":            d.dataAuthOK.Load(),
		"data_auth_failure_count":            d.dataAuthFailed.Load(),
		"data_session_failure_count":         d.dataSessionFailed.Load(),
		"data_canceled_count":                d.dataCanceled.Load(),
		"data_error_transport_timeout_count": d.dataTimeout.Load(),
		"data_error_proof_invalid_count":     d.dataProofInvalid.Load(),
		"data_error_session_not_ready_count": d.dataSessionNotReady.Load(),
		"data_error_lease_conflict_count":    d.dataLeaseConflict.Load(),
		"data_error_auth_required_count":     d.dataAuthRequired.Load(),
		"data_error_other_count":             d.dataOther.Load(),
		"vpn_stage":                          vpnStage,
		"vpn_error_class":                    vpnError,
		"vpn_auth_code":                      vpnAuthCode,
	}
}

func (d *managedDiagnostics) terminalMessage() bridgeMessage {
	message := d.message()
	message["vpn_terminal"] = true
	return message
}

func (b *managedBridge) emitDiagnostics(ctx context.Context, d *managedDiagnostics) {
	tick := time.NewTimer(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if ctx.Err() != nil || b.send(d.message()) != nil {
				return
			}
			// Reset after the write: a slow pipe must not cause catch-up bursts.
			tick.Reset(time.Second)
		}
	}
}
