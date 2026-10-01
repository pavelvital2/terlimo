package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cbeuw/connutil"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/turn/v5"
)

// ManagedTransportConfig is private, per-Connect state. Authentication must use
// the exporter of the supplied live connection, never a cached bearer token.
// The child binds each Go socket explicitly; process binding covers libc DNS.
type ManagedTransportConfig struct {
	NodePin              string
	RegistrationDeviceID string
	TransportSession     string
	Authenticate         func(context.Context, *dtls.Conn, string, int) error
	Diagnostics          *managedDiagnostics
}

type managedTransportKey struct{}

func WithManagedTransport(ctx context.Context, config *ManagedTransportConfig) context.Context {
	// A nil configuration still selects managed mode and fails closed.
	copyConfig := ManagedTransportConfig{}
	if config != nil {
		copyConfig = *config
	}
	return context.WithValue(ctx, managedTransportKey{}, &copyConfig)
}

func managedTransport(ctx context.Context) *ManagedTransportConfig {
	config, _ := ctx.Value(managedTransportKey{}).(*ManagedTransportConfig)
	return config
}

func transportLogger(ctx context.Context) *log.Logger {
	if managedTransport(ctx) != nil {
		return log.New(io.Discard, "", 0)
	}
	return log.Default()
}

// Keep errors safe even if a caller accidentally logs one. No wrapped URL,
// server-controlled reason, password, certificate, or device identifier escapes.
func managedSafeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := workerPolicyLimit(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("TRANSPORT_TIMEOUT")
	}
	switch err.Error() {
	case "TRUST_FAILED", "AUTH_REQUIRED", "PROOF_INVALID", "GRANT_REVOKED", "LEASE_EXPIRED", "UNSUPPORTED_AUTH", "KEY_UNAVAILABLE", "SESSION_NOT_READY", "LEASE_CONFLICT", "TRANSPORT_TIMEOUT", "TRANSPORT_FAILED", "CONFIG_REQUIRED", "BAD_MESSAGE", "TURN_EXPIRED", "TURN_CAPACITY", "VK_API_UNAVAILABLE", "VK_CAPTCHA_REQUIRED", "VK_FLOOD":
		return errors.New(err.Error())
	}
	return errors.New("TRANSPORT_FAILED")
}

func managedTerminalError(err error) bool {
	if err == nil {
		return false
	}
	switch managedSafeError(err).Error() {
	case "TRUST_FAILED", "AUTH_REQUIRED", "PROOF_INVALID", "GRANT_REVOKED", "LEASE_EXPIRED", "UNSUPPORTED_AUTH", "KEY_UNAVAILABLE", "BAD_MESSAGE":
		return true
	}
	return false
}

func managedPinVerifier(pin string) (func([][]byte, [][]*x509.Certificate) error, error) {
	expected, err := base64.RawURLEncoding.Strict().DecodeString(pin)
	if err != nil || len(expected) != sha256.Size || base64.RawURLEncoding.EncodeToString(expected) != pin {
		return nil, errors.New("TRUST_FAILED")
	}
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("TRUST_FAILED")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return errors.New("TRUST_FAILED")
		}
		actual := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		if subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
			return errors.New("TRUST_FAILED")
		}
		return nil
	}, nil
}

// BootstrapTransportConfig never enters WorkerGroup or the native data plane.
// The caller owns returned cleanup, including cancellation after handshake.
type BootstrapTransportConfig struct {
	Peer     *net.UDPAddr
	Password string
	Hashes   []string
	Pin      string
	StreamID int
}

func DialBootstrapTransport(ctx context.Context, config BootstrapTransportConfig) (*dtls.Conn, func(), error) {
	if _, err := managedPinVerifier(config.Pin); err != nil {
		return nil, nil, err
	}
	if config.Peer == nil || len(config.Hashes) == 0 || len(config.Hashes) > 4 || config.Password == "" || config.StreamID < 0 {
		return nil, nil, errors.New("BAD_MESSAGE")
	}
	key, err := deriveWrapKey(config.Password)
	if err != nil {
		return nil, nil, errors.New("BAD_MESSAGE")
	}
	// Canonical v17 VKCalls/legacy-fallback and credential cache, not a new API path.
	user, pass, urls, err := GetCreds(ctx, config.Hashes[0], config.StreamID)
	for i := 1; err != nil && i < len(config.Hashes) && isHashFallbackCredentialError(err); i++ {
		user, pass, urls, err = GetCreds(ctx, config.Hashes[i], config.StreamID)
	}
	if err != nil {
		return nil, nil, errors.New("VK_API_UNAVAILABLE")
	}
	tp := &TurnParams{WrapKey: key, Hashes: config.Hashes}
	creds := &Credentials{User: user, Pass: pass, TurnURLs: urls, CacheStreamID: config.StreamID}
	return dialManagedTransport(ctx, tp, config.Peer, creds, config.Pin, config.StreamID, false, 0, false, nil)
}

// dialManagedTransport only establishes TURN -> WRAP -> DTLS. No app record,
// GETCONF, READY, MUX, WireGuard packet or external MASQUE fallback is sent here.
func dialManagedTransport(ctx context.Context, tp *TurnParams, peer *net.UDPAddr, creds *Credentials,
	pin string, workerID int, preferStream bool, retry int, observeVPN bool, onAllocated func()) (*dtls.Conn, func(), error) {
	trace := mobileServiceDialTrace(ctx)
	diagnostic := bootstrapTrace(ctx)
	var vpnDiagnostic *managedDiagnostics
	if managed := managedTransport(ctx); observeVPN && managed != nil {
		vpnDiagnostic = managed.Diagnostics
	}
	verify, err := managedPinVerifier(pin)
	if err != nil {
		return nil, nil, err
	}
	if tp == nil || peer == nil || peer.IP == nil || peer.Port < 1 || peer.Port > 65535 || creds == nil || len(tp.WrapKey) != wrapKeyLen {
		return nil, nil, errors.New("BAD_MESSAGE")
	}
	candidates := sessionTURNCandidatesForAttempt(creds.TurnURLs, workerID, retry, tp, preferStream)
	var relay net.PacketConn
	var allocationClose func()
	lastCode := "TRANSPORT_FAILED"
	allocatedOrdinal := 0
	var allocatedTransport turnTransport
	for candidateIndex, candidate := range candidates {
		ordinal := candidateIndex + 1
		trace.note(dialCandidateBegin, ordinal, candidate.Transport, nil, true)
		if ctx.Err() != nil {
			trace.note(dialCandidateEnd, ordinal, candidate.Transport, ctx.Err(), false)
			return nil, nil, context.Canceled
		}
		// Context controls dials. Closing the physical socket also interrupts a
		// pending STUN Allocate; TURN owns neither cancellation nor raw socket.
		dialer := managedNetworkDialer(8 * time.Second)
		network := "tcp"
		if candidate.Transport == turnTransportUDP {
			network = "udp"
		}
		trace.note(dialSocketBegin, ordinal, candidate.Transport, nil, true)
		raw, openErr := dialer.DialContext(ctx, network, candidate.address())
		trace.note(dialSocketEnd, ordinal, candidate.Transport, openErr, false)
		if openErr != nil {
			trace.note(dialCandidateEnd, ordinal, candidate.Transport, openErr, false)
			continue
		}
		stopRaw := context.AfterFunc(ctx, func() { _ = raw.Close() })
		closeRaw := func() { stopRaw(); _ = raw.Close() }
		var packet net.PacketConn
		if candidate.Transport == turnTransportUDP {
			udp, ok := raw.(*net.UDPConn)
			if !ok {
				trace.note(dialCandidateEnd, ordinal, candidate.Transport, errors.New("TRANSPORT_FAILED"), false)
				closeRaw()
				continue
			}
			_ = udp.SetReadBuffer(socketBufSize)
			_ = udp.SetWriteBuffer(socketBufSize)
			packet = &connectedUDPConn{udp}
		} else {
			stream := raw
			if candidate.Transport == turnTransportTLS {
				secure := tls.Client(raw, turnTLSConfig(candidate, tp.TLSFrontSNI))
				handshakeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
				trace.note(dialTLSBegin, ordinal, candidate.Transport, nil, true)
				openErr = secure.HandshakeContext(handshakeCtx)
				trace.note(dialTLSEnd, ordinal, candidate.Transport, openErr, false)
				cancel()
				if openErr != nil {
					trace.note(dialCandidateEnd, ordinal, candidate.Transport, openErr, false)
					closeRaw()
					continue
				}
				stream = secure
			} else if preferStream {
				stream = &splitFirstWriteConn{Conn: raw, splitAt: 6, delay: 20 * time.Millisecond}
			}
			packet = turn.NewSTUNConn(stream)
		}
		_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
		tc, allocated, allocErr := allocateTURNOnConnObserved(candidate, peer, creds, packet, trace, ordinal)
		if allocErr != nil {
			trace.note(dialCandidateEnd, ordinal, candidate.Transport, allocErr, false)
			closeRaw()
			if isCredentialTURNError(allocErr) {
				lastCode = "TURN_EXPIRED"
			}
			if isTURNCapacityError(allocErr) {
				lastCode = "TURN_CAPACITY"
			}
			continue
		}
		_ = raw.SetDeadline(time.Time{})
		relay = allocated
		allocatedOrdinal, allocatedTransport = ordinal, candidate.Transport
		trace.note(dialCandidateEnd, ordinal, candidate.Transport, nil, false)
		allocationClose = func() { _ = relay.Close(); tc.Close(); closeRaw() }
		break
	}
	if relay == nil {
		return nil, nil, errors.New(lastCode)
	}
	if onAllocated != nil {
		onAllocated()
	}
	if trace != nil {
		trace.allocation(relay.LocalAddr(), allocatedOrdinal, allocatedTransport)
	}
	relay = observeFirstServiceDialWrite(relay, trace, allocatedOrdinal, allocatedTransport)
	pipeA, pipeB := connutil.AsyncPacketPipe()
	connCtx, cancel := context.WithCancel(ctx)
	diagnostic.setTransport(connCtx)
	var conn *dtls.Conn
	var once sync.Once
	var wg sync.WaitGroup
	closeResources := func() { cancel(); _ = pipeA.Close(); _ = pipeB.Close(); allocationClose() }
	stop := context.AfterFunc(connCtx, func() { once.Do(closeResources) })
	cleanup := func() {
		diagnostic.noteClose("CLEANUP", nil)
		once.Do(closeResources)
		stop()
		if conn != nil {
			_ = conn.Close()
		}
		wg.Wait()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()
		buf, plain := make([]byte, readBufSize+80), make([]byte, readBufSize)
		for {
			n, from, err := relay.ReadFrom(buf)
			if err != nil {
				trace.ioNote(ioReadError)
				diagnostic.noteClose("RELAY_READ", err)
				return
			}
			if err = serviceRelayHandoff(trace, from, peer, tp.WrapKey, buf[:n], plain, pipeA); err != nil {
				diagnostic.noteClose("PIPE_WRITE", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		buf := make([]byte, readBufSize)
		obfsConfig, state := NewObfsConfig(), NewObfsState()
		for {
			n, _, err := pipeA.ReadFrom(buf)
			if err != nil {
				trace.ioNote(ioTXReadError)
				diagnostic.noteClose("PIPE_READ", err)
				return
			}
			wrapped, err := obfsWrapPacket(tp.WrapKey, buf[:n], obfsConfig, state)
			if err != nil {
				trace.ioNote(ioWrapError)
				diagnostic.noteClose("UNKNOWN", err)
				return
			}
			if _, err = serviceRelayWrite(trace, relay, wrapped, peer); err != nil {
				diagnostic.noteClose("RELAY_WRITE", err)
				return
			}
		}
	}()
	trace.note(dialCertBegin, allocatedOrdinal, allocatedTransport, nil, true)
	cert, err := selfsign.GenerateSelfSigned()
	trace.note(dialCertEnd, allocatedOrdinal, allocatedTransport, err, false)
	if err != nil {
		cleanup()
		return nil, nil, errors.New("TRANSPORT_FAILED")
	}
	var trustFailed atomic.Bool
	dtlsConfig := &dtls.Config{
		Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, chains [][]*x509.Certificate) error {
			err := verify(raw, chains)
			if err != nil {
				trustFailed.Store(true)
			}
			return err
		},
		ExtendedMasterSecret:  dtls.RequireExtendedMasterSecret,
		CipherSuites:          []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		ConnectionIDGenerator: dtls.OnlySendCIDGenerator(), LoggerFactory: &NullLoggerFactory{},
	}
	trace.note(dialSemaphoreWait, allocatedOrdinal, allocatedTransport, nil, true)
	select {
	case handshakeSem <- struct{}{}:
		trace.note(dialSemaphoreAcquired, allocatedOrdinal, allocatedTransport, nil, false)
	case <-connCtx.Done():
		trace.note(dialSemaphoreEnd, allocatedOrdinal, allocatedTransport, connCtx.Err(), false)
		cleanup()
		return nil, nil, context.Canceled
	}
	diagnostic.setStage("HANDSHAKE", false)
	vpnDiagnostic.noteVPN(vpnStageHandshake, nil)
	trace.note(dialDTLSBegin, allocatedOrdinal, allocatedTransport, nil, true)
	conn, err = dtls.Client(pipeB, peer, dtlsConfig)
	if err == nil {
		handshakeCtx, stopHandshake := context.WithTimeout(connCtx, wrapHandshakeTimeout)
		err = conn.HandshakeContext(handshakeCtx)
		stopHandshake()
	}
	trace.note(dialDTLSEnd, allocatedOrdinal, allocatedTransport, err, false)
	<-handshakeSem
	if err != nil {
		vpnDiagnostic.noteVPN(vpnStageHandshake, err)
		if diagnostic != nil {
			_ = diagnostic.finish(err)
		}
		cleanup()
		if trustFailed.Load() {
			return nil, nil, errors.New("TRUST_FAILED")
		}
		if ctx.Err() != nil {
			return nil, nil, context.Canceled
		}
		return nil, nil, errors.New("TRANSPORT_TIMEOUT")
	}
	diagnostic.setStage("HANDSHAKE", true)
	vpnDiagnostic.noteVPN(vpnStageAuth, nil)
	// HandshakeContext cancellation alone does not close an established DTLS
	// connection; explicitly close it even if a host callback is still pending.
	stopDTLS := context.AfterFunc(connCtx, func() { _ = conn.Close() })
	return conn, func() { cleanup(); stopDTLS() }, nil
}

func runManagedSession(ctx context.Context, tp *TurnParams, peer *net.UDPAddr, d *Dispatcher,
	localPort string, getConfig bool, configCh chan<- string, onConfigDelivered func(), workerID int,
	creds *Credentials, password string, stats *Stats, preferStream bool, retry int, onAllocated func()) (bool, error) {
	config := managedTransport(ctx)
	if config == nil || config.Authenticate == nil || config.RegistrationDeviceID == "" ||
		strings.ContainsAny(config.RegistrationDeviceID, "|\r\n") || config.TransportSession == "" ||
		normalizeTransportSession(config.TransportSession) != config.TransportSession || password == "" ||
		(getConfig && configCh == nil) {
		return false, errors.New("AUTH_REQUIRED")
	}
	port, err := strconv.Atoi(localPort)
	if err != nil || port < 1 || port > 65535 || strings.ContainsAny(password, "|\r\n") {
		return false, errors.New("BAD_MESSAGE")
	}
	config.Diagnostics.noteWorker(getConfig, false)
	conn, cleanup, err := dialManagedTransport(ctx, tp, peer, creds, config.NodePin, workerID, preferStream, retry, getConfig, onAllocated)
	config.Diagnostics.noteDataStage(getConfig, managedDataDial, err)
	if err != nil {
		return false, managedSafeError(err)
	}
	defer cleanup()
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	mode := "data"
	if getConfig {
		mode = "getconf"
	}
	delivered, err := managedAuthenticateAndRun(sessionCtx, conn, config, mode, workerID, func() (bool, error) {
		return runConnectedSession(sessionCtx, cancel, conn, d, localPort, getConfig, configCh, getConfig,
			onConfigDelivered, workerID, config.RegistrationDeviceID, password, "", config.TransportSession, stats)
	})
	return delivered, managedSafeError(err)
}

// Keep every post-auth action behind the same gate, including config delivery,
// dispatcher registration, keepalive and the native multipath marker.
func managedAuthenticateAndRun(ctx context.Context, conn *dtls.Conn, config *ManagedTransportConfig,
	mode string, workerID int, next func() (bool, error)) (bool, error) {
	var diagnostic *managedDiagnostics
	if config != nil {
		diagnostic = config.Diagnostics
	}
	if ctx.Err() != nil {
		if mode == "getconf" {
			diagnostic.noteVPN(vpnStageAuth, context.Canceled)
		}
		diagnostic.noteDataStage(mode != "data", managedDataAuth, context.Canceled)
		return false, context.Canceled
	}
	if config == nil || config.Authenticate == nil {
		if mode == "getconf" {
			diagnostic.noteVPN(vpnStageAuth, errors.New("AUTH_REQUIRED"))
		}
		diagnostic.noteDataStage(mode != "data", managedDataAuth, errors.New("AUTH_REQUIRED"))
		return false, errors.New("AUTH_REQUIRED")
	}
	if mode != "getconf" && mode != "data" {
		return false, errors.New("BAD_MESSAGE")
	}
	if err := config.Authenticate(ctx, conn, mode, workerID); err != nil {
		if mode == "getconf" {
			diagnostic.noteVPN(vpnStageAuth, err)
		}
		diagnostic.noteDataStage(mode != "data", managedDataAuth, err)
		return false, managedSafeError(err)
	}
	if ctx.Err() != nil {
		diagnostic.noteDataStage(mode != "data", managedDataAuth, context.Canceled)
		return false, context.Canceled
	}
	diagnostic.noteDataStage(mode != "data", managedDataAuth, nil)
	if mode == "getconf" {
		diagnostic.noteVPN(vpnStageConfig, nil)
	}
	delivered, err := next()
	diagnostic.noteDataStage(mode != "data", managedDataSession, err)
	return delivered, err
}
