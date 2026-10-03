package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/pion/dtls/v3"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
	"wg-turn-client/internal/wlwire"
)

type clientAuthBody struct {
	V         int    `json:"v"`
	Op        string `json:"op"`
	Challenge string `json:"challenge_id,omitempty"`
	Payload   string `json:"payload_b64,omitempty"`
	Proof     string `json:"proof_b64,omitempty"`
}
type clientAuthPayload struct {
	Op           string `json:"op"`
	Node         string `json:"node_id"`
	Grant        string `json:"grant_id"`
	Registration string `json:"registration_id"`
	Generation   string `json:"generation"`
	Seq          string `json:"lease_seq"`
	Session      string `json:"transport_session"`
	Worker       string `json:"worker_id"`
	Mode         string `json:"mode"`
}

var clientTestWorkers = struct {
	sync.Mutex
	workers  map[string]map[*clientTestConn]bool
	sessions map[string]string
}{workers: map[string]map[*clientTestConn]bool{}, sessions: map[string]string{}}

type clientTestConn struct {
	trace *managedConnTrace
	net.Conn
	identity       accessIdentity
	grant          ClientTestGrant
	auth           clientAuthPayload
	authID         wlwire.ID
	authBody       []byte
	authOK         []byte
	authTranscript []byte
	once           sync.Once
	done           chan struct{}
	configured     bool
}

func clientTestWorkerCount(g string) int {
	clientTestWorkers.Lock()
	defer clientTestWorkers.Unlock()
	return len(clientTestWorkers.workers[g])
}
func clientTestCloseGrant(g string) {
	clientTestWorkers.Lock()
	list := []*clientTestConn{}
	for c := range clientTestWorkers.workers[g] {
		list = append(list, c)
	}
	delete(clientTestWorkers.sessions, g)
	clientTestWorkers.Unlock()
	for _, c := range list {
		_ = c.Close()
	}
}
func (c *clientTestConn) Close() error {
	c.once.Do(func() {
		c.trace.exit("close", net.ErrClosed)
		close(c.done)
		clientTestWorkers.Lock()
		delete(clientTestWorkers.workers[c.grant.GrantID], c)
		clientTestWorkers.Unlock()
		_ = c.Conn.Close()
	})
	return nil
}
func (c *clientTestConn) Write(b []byte) (int, error) {
	if !clientTestCurrentGrantActive(c.identity, c.grant.Generation, time.Now().Unix()) {
		err := errors.New("LEASE_EXPIRED")
		if c.trace != nil {
			c.trace.exit("write_rejected", err)
		}
		return 0, err
	}
	n, e := c.Conn.Write(b)
	if e != nil {
		c.trace.exit("write", e)
	}
	if e == nil && c.auth.Mode == "getconf" && bytes.Contains(b, []byte("[Interface]")) {
		clientTestWorkers.Lock()
		clientTestWorkers.sessions[c.grant.GrantID] = c.auth.Session
		clientTestWorkers.Unlock()
		c.configured = true
		c.trace.event("config_written", "none")
	}
	return n, e
}
func (c *clientTestConn) Read(b []byte) (int, error) {
	for {
		n, e := c.Conn.Read(b)
		if e != nil {
			c.trace.exit("read", e)
			return n, e
		}
		if n >= 4 && string(b[:4]) == "WLBS" {
			var a wlwire.Assembler
			deadline := time.Now().Add(10 * time.Second)
			_ = c.Conn.SetReadDeadline(deadline)
			var id wlwire.ID
			var body []byte
			var err error
			for {
				if n < 6 || b[5] != 0 {
					return 0, wlwire.ErrMessage
				}
				id, body, err = a.Add(b[:n], time.Now())
				if err != nil || body != nil {
					break
				}
				n, err = c.Conn.Read(b)
				if err != nil {
					break
				}
			}
			_ = c.Conn.SetReadDeadline(time.Time{})
			if err != nil || body == nil || id != c.authID {
				return 0, c.readFailure("AUTH_REQUIRED")
			}
			var auth clientAuthBody
			if wlwire.StrictJSON(body, &auth) != nil {
				return 0, c.readFailure("AUTH_REQUIRED")
			}
			var original clientAuthBody
			_ = wlwire.StrictJSON(c.authBody, &original)
			key, _ := decodeClientB64(c.grant.PublicKey)
			sig, sigErr := decodeClientB64(auth.Proof)
			if auth.V != 1 || auth.Op != "VPN_AUTH" || auth.Payload != original.Payload || auth.Challenge != original.Challenge || sigErr != nil || wlwire.Verify(key, c.authTranscript, sig) != nil {
				return 0, c.readFailure("PROOF_INVALID")
			}
			if err = clientWriteBody(c.Conn, id, c.authOK); err != nil {
				return 0, err
			}
			continue
		}
		if !c.configured {
			if c.auth.Mode == "getconf" {
				parts := bytes.Split(bytes.TrimPrefix(b[:n], []byte("GETCONF:")), []byte("|"))
				if !bytes.HasPrefix(b[:n], []byte("GETCONF:")) || len(parts) != 5 || string(parts[1]) != c.grant.RegistrationID || string(parts[2]) != c.identity.password || string(parts[4]) != c.auth.Session {
					return 0, c.readFailure("AUTH_REQUIRED")
				}
			} else {
				clientTestWorkers.Lock()
				ready := clientTestWorkers.sessions[c.grant.GrantID] == c.auth.Session
				clientTestWorkers.Unlock()
				if !ready {
					return 0, c.readFailure("SESSION_NOT_READY")
				}
				c.configured = true
			}
		}
		if !clientTestCurrentGrantActive(c.identity, c.grant.Generation, time.Now().Unix()) {
			return 0, c.readFailure("LEASE_EXPIRED")
		}
		c.trace.acceptedRead()
		return n, nil
	}
}
func (c *clientTestConn) readFailure(code string) error {
	err := errors.New(code)
	c.trace.exit("read_rejected", err)
	return err
}
func (c *clientTestConn) SetDeadline(deadline time.Time) error {
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		c.trace.event("forced_deadline", "none")
	}
	return c.Conn.SetDeadline(deadline)
}
func clientReadBody(c net.Conn) (wlwire.ID, []byte, error) {
	return clientReadBodyUntil(c, time.Now().Add(10*time.Second), nil)
}

// echo is available only before the first AUTH fragment; subsequent reads stay strict.
func clientReadBodyUntil(c net.Conn, deadline time.Time, echo func() error) (wlwire.ID, []byte, error) {
	var a wlwire.Assembler
	_ = c.SetReadDeadline(deadline)
	defer c.SetReadDeadline(time.Time{})
	b := make([]byte, 32+wlwire.Fragment+1)
	for {
		n, e := c.Read(b)
		if e != nil {
			return wlwire.ID{}, nil, e
		}
		if echo != nil && isExactKeepalive(b[:n]) {
			if e = echo(); e != nil {
				return wlwire.ID{}, nil, e
			}
			continue
		}
		echo = nil
		if n < 6 || b[5] != 0 {
			return wlwire.ID{}, nil, wlwire.ErrMessage
		}
		id, body, e := a.Add(b[:n], time.Now())
		if e != nil || body != nil {
			return id, body, e
		}
	}
}

// Snapshot only: never retain dbMutex across network I/O. WRAP cache admission
// alone does not authorize echo, nor does echo authorize any VPN traffic.
func clientTestEchoSnapshot(identity accessIdentity, expected ClientTestGrant) (int64, bool) {
	dbMutex.Lock()
	defer dbMutex.Unlock()
	if !identity.valid() || identity.isMain || identity.isService {
		return 0, false
	}
	for _, bootstrap := range db.ClientBootstrap {
		if bootstrap.Secret == identity.password {
			return 0, false
		}
	}
	entry := db.Passwords[identity.password]
	if entry == nil || entry.IsDeactivated || entry.ClientTest == nil {
		return 0, false
	}
	g := entry.ClientTest
	return entry.ExpiresAt, clientTestExpiryActive(entry.ExpiresAt, time.Now().Unix()) &&
		!g.Revoked && g.NodeID == clientTestNodeID && g.NodeID == expected.NodeID &&
		g.GrantID == expected.GrantID && g.RegistrationID == expected.RegistrationID &&
		g.Generation == expected.Generation && g.LeaseSeq == expected.LeaseSeq
}

func clientReadInitialBody(ctx context.Context, c net.Conn, identity accessIdentity, g ClientTestGrant) (wlwire.ID, []byte, error) {
	deadline := time.Now().Add(10 * time.Second)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	if expiry, _ := clientTestEchoSnapshot(identity, g); expiry != 0 && time.Unix(expiry, 0).Before(deadline) {
		deadline = time.Unix(expiry, 0)
	}
	echoed := false
	return clientReadBodyUntil(c, deadline, func() error {
		if echoed {
			return wlwire.ErrMessage
		}
		expiry, active := clientTestEchoSnapshot(identity, g)
		if !active {
			return errors.New("AUTH_REQUIRED")
		}
		until := deadline
		if expiry != 0 && time.Unix(expiry, 0).Before(until) {
			until = time.Unix(expiry, 0)
		}
		if ctx.Err() != nil || !time.Now().Before(until) {
			return context.DeadlineExceeded
		}
		writeUntil := time.Now().Add(5 * time.Second)
		if until.Before(writeUntil) {
			writeUntil = until
		}
		_ = c.SetWriteDeadline(writeUntil)
		defer c.SetWriteDeadline(time.Time{})
		echoed = true
		n, err := c.Write([]byte{0xff})
		if err == nil && n != 1 {
			err = io.ErrShortWrite
		}
		return err
	})
}

func clientWriteBody(c net.Conn, id wlwire.ID, b []byte) error {
	fs, e := wlwire.Frames(id, true, b)
	if e != nil {
		return e
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer c.SetWriteDeadline(time.Time{})
	for _, f := range fs {
		if _, e = c.Write(f); e != nil {
			return e
		}
	}
	return nil
}
func clientWriteJSON(c net.Conn, id wlwire.ID, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return clientWriteBody(c, id, b)
}
func clientAuthError(c net.Conn, id wlwire.ID, code string) {
	_ = clientWriteJSON(c, id, map[string]any{"v": 1, "op": "VPN_AUTH_ERROR", "status": "error", "code": code, "retryable": code == "LEASE_CONFLICT" || code == "CHALLENGE_EXPIRED" || code == "SESSION_NOT_READY"})
}
func clientTestAuthenticate(ctx context.Context, c *dtls.Conn, identity accessIdentity, g ClientTestGrant, wgDev wgDevice) (secured net.Conn, authErr error) {
	trace := newManagedConnTrace()
	defer func() {
		if authErr != nil {
			trace.exit("auth_failed", authErr)
		}
	}()
	state, ok := c.ConnectionState()
	if !ok {
		return nil, errors.New("AUTH_REQUIRED")
	}
	exporter, e := state.ExportKeyingMaterial("EXPORTER-WL-VPN-POP-1", nil, 32)
	if e != nil {
		return nil, e
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	for attempt := 0; attempt < 3; attempt++ {
		var beginID wlwire.ID
		var b []byte
		if attempt == 0 {
			beginID, b, e = clientReadInitialBody(ctx, c, identity, g)
		} else {
			beginID, b, e = clientReadBody(c)
		}
		if e != nil {
			return nil, e
		}
		var begin clientAuthBody
		if wlwire.StrictJSON(b, &begin) != nil || begin.V != 1 || begin.Op != "VPN_AUTH_BEGIN" || begin.Payload != "" || begin.Proof != "" || begin.Challenge != "" {
			clientAuthError(c, beginID, "AUTH_REQUIRED")
			return nil, errors.New("AUTH_REQUIRED")
		}
		current, expiry, found := clientTestGrantFor(identity)
		if !found || current.Revoked || current.Generation != g.Generation {
			clientAuthError(c, beginID, "GRANT_REVOKED")
			return nil, errors.New("GRANT_REVOKED")
		}
		if !clientTestExpiryActive(expiry, time.Now().Unix()) {
			clientAuthError(c, beginID, "LEASE_EXPIRED")
			return nil, errors.New("LEASE_EXPIRED")
		}
		g = current
		challenge := make([]byte, 16)
		nonce := make([]byte, 32)
		_, _ = rand.Read(challenge)
		_, _ = rand.Read(nonce)
		until := time.Now().Add(15 * time.Second)
		wireVersion := 1
		var accessExpiresAt any = time.Unix(expiry, 0).UTC().Format(time.RFC3339)
		if expiry == 0 {
			wireVersion = 2
			accessExpiresAt = nil
		}
		reply := map[string]any{"v": wireVersion, "op": "VPN_CHALLENGE", "status": "ok", "challenge_id": wlwire.Encode(challenge), "nonce": wlwire.Encode(nonce), "grant_id": g.GrantID, "registration_id": g.RegistrationID, "node_id": g.NodeID, "generation": g.Generation, "lease_seq": g.LeaseSeq, "challenge_expires_at": until.UTC().Format(time.RFC3339Nano), "access_expires_at": accessExpiresAt, "server_time": time.Now().UTC().Format(time.RFC3339Nano)}
		if e = clientWriteJSON(c, beginID, reply); e != nil {
			return nil, e
		}
		for duplicate := 0; duplicate < 3; duplicate++ {
			authID, body, e := clientReadBody(c)
			if e != nil {
				return nil, e
			}
			if authID == beginID && bytes.Equal(body, b) && time.Now().Before(until) {
				if e = clientWriteJSON(c, beginID, reply); e != nil {
					return nil, e
				}
				continue
			}
			var auth clientAuthBody
			var p clientAuthPayload
			if authID == beginID || wlwire.StrictJSON(body, &auth) != nil || auth.V != 1 || auth.Op != "VPN_AUTH" || auth.Challenge != wlwire.Encode(challenge) {
				clientAuthError(c, authID, "PROOF_INVALID")
				return nil, wlwire.ErrProof
			}
			payload, e := decodeClientB64(auth.Payload)
			if e != nil || wlwire.StrictJSON(payload, &p) != nil {
				return nil, wlwire.ErrProof
			}
			sig, e := decodeClientB64(auth.Proof)
			if e != nil {
				return nil, e
			}
			key, _ := decodeClientB64(g.PublicKey)
			if p.Op != "vpn_auth" || p.Grant != g.GrantID || p.Registration != g.RegistrationID || p.Node != g.NodeID || p.Generation != g.Generation || p.Seq != g.LeaseSeq || !validTransportSession(p.Session) || !clientTestWorkerValid(p.Worker, p.Mode, configuredAccessWorkerLimit()) || wlwire.Verify(key, wlwire.Transcript("WL-VPN-POP-1", exporter, challenge, nonce, payload), sig) != nil {
				clientAuthError(c, authID, "PROOF_INVALID")
				return nil, wlwire.ErrProof
			}
			if !time.Now().Before(until) {
				clientAuthError(c, authID, "CHALLENGE_EXPIRED")
				break
			}
			// Hold the authoritative grant lock through admission, fencing concurrent revoke.
			dbMutex.Lock()
			entry := db.Passwords[identity.password]
			if entry == nil || entry.ClientTest == nil || entry.ClientTest.Revoked || entry.ClientTest.Generation != g.Generation {
				dbMutex.Unlock()
				clientAuthError(c, authID, "GRANT_REVOKED")
				return nil, errors.New("GRANT_REVOKED")
			}
			if !clientTestExpiryActive(entry.ExpiresAt, time.Now().Unix()) {
				dbMutex.Unlock()
				clientAuthError(c, authID, "LEASE_EXPIRED")
				return nil, errors.New("LEASE_EXPIRED")
			}
			if entry.ClientTest.LeaseSeq != g.LeaseSeq {
				dbMutex.Unlock()
				clientAuthError(c, authID, "LEASE_CONFLICT")
				break
			}
			clientTestWorkers.Lock()
			if p.Mode == "data" && clientTestWorkers.sessions[g.GrantID] != p.Session {
				clientTestWorkers.Unlock()
				dbMutex.Unlock()
				clientAuthError(c, authID, "SESSION_NOT_READY")
				return nil, errors.New("SESSION_NOT_READY")
			}
			for worker := range clientTestWorkers.workers[g.GrantID] {
				if worker.auth.Session == p.Session && worker.auth.Worker == p.Worker {
					clientTestWorkers.Unlock()
					dbMutex.Unlock()
					clientAuthError(c, authID, "SESSION_NOT_READY")
					return nil, errors.New("SESSION_NOT_READY")
				}
			}
			mc := &clientTestConn{trace: trace, Conn: c, identity: identity, grant: g, auth: p, authID: authID, authBody: body, authTranscript: wlwire.Transcript("WL-VPN-POP-1", exporter, challenge, nonce, payload), done: make(chan struct{})}
			if clientTestWorkers.workers[g.GrantID] == nil {
				clientTestWorkers.workers[g.GrantID] = map[*clientTestConn]bool{}
			}
			clientTestWorkers.workers[g.GrantID][mc] = true
			clientTestWorkers.Unlock()
			dbMutex.Unlock()
			answer := map[string]any{"v": reply["v"], "op": "VPN_AUTH_OK", "status": "ok", "challenge_id": auth.Challenge, "grant_id": g.GrantID, "registration_id": g.RegistrationID, "node_id": g.NodeID, "generation": g.Generation, "lease_seq": g.LeaseSeq, "transport_session": p.Session, "worker_id": p.Worker, "mode": p.Mode, "access_expires_at": reply["access_expires_at"], "server_time": reply["server_time"]}
			mc.authOK, _ = json.Marshal(answer)
			if e = clientWriteBody(c, authID, mc.authOK); e != nil {
				trace.exit("auth_reply_write", e)
				_ = mc.Close()
				return nil, e
			}
			go func() {
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-mc.done:
						return
					case <-ctx.Done():
						trace.exit("auth_context_done", ctx.Err())
						_ = mc.Close()
						return
					case <-ticker.C:
						dbMutex.Lock()
						entry := db.Passwords[identity.password]
						expired := entry == nil || entry.ClientTest == nil || entry.ClientTest.Revoked || entry.ClientTest.Generation != g.Generation || !clientTestExpiryActive(entry.ExpiresAt, time.Now().Unix())
						if expired {
							trace.event("watchdog_access_inactive", "access_inactive")
							_ = clientTestRemovePeer(wgDev, g)
							clientTestCloseGrant(g.GrantID)
						}
						dbMutex.Unlock()
						if expired {
							return
						}
					}
				}
			}()
			trace.event("auth_ok", "none")
			return mc, nil
		}
	}
	return nil, errors.New("AUTH_REQUIRED")
}

// Bootstrap is a bounded, fixed-destination RPC tunnel, never a generic relay.
func clientTestBootstrapServe(ctx context.Context, c net.Conn, b ClientTestBootstrap) {
	started := time.Now()
	shape := "none"
	trace := func(stage, class string) {
		log.Printf("[BOOTSTRAP_TRACE] stage=%s class=%s elapsed_ms=%d response_shape=%s", stage, class, time.Since(started).Milliseconds(), shape)
	}
	trace("started", "none")
	session := make([]byte, 16)
	_, _ = rand.Read(session)
	defer c.Close()
	for rpc := 0; rpc < 12; rpc++ {
		shape = "none"
		if b.Revoked || !clientTestExpiryActive(b.ExpiresAt, time.Now().Unix()) {
			trace("bootstrap_initial_access", "access_inactive")
			return
		}
		trace("bootstrap_initial_access", "none")
		id, body, e := clientReadBody(c)
		if e != nil {
			trace("bootstrap_request_read", managedErrorClass(e))
			return
		}
		trace("bootstrap_request_read", "none")
		current, ok := clientTestBootstrapFor(accessIdentity{password: b.Secret})
		if !ok || current.Revoked || !clientTestExpiryActive(current.ExpiresAt, time.Now().Unix()) {
			trace("bootstrap_current_access", "access_inactive")
			return
		}
		trace("bootstrap_current_access", "none")
		path := os.Getenv("WL_TEST_BACKEND_SOCKET")
		if path == "" || path[0] != '/' {
			trace("bootstrap_backend_path", "invalid")
			return
		}
		d := net.Dialer{Timeout: 3 * time.Second}
		up, e := d.DialContext(ctx, "unix", path)
		if e != nil {
			trace("bootstrap_backend_dial", managedErrorClass(e))
			_ = clientWriteJSON(c, id, map[string]any{"v": 1, "status": "error", "code": "BACKEND_UNAVAILABLE", "retryable": true})
			continue
		}
		trace("bootstrap_backend_dial", "none")
		_ = up.SetDeadline(time.Now().Add(15 * time.Second))
		envelope := map[string]any{"credential_id": b.CredentialID, "connection_id": hex.EncodeToString(session), "request_id": wlwire.Encode(id[:]), "body": json.RawMessage(body)}
		e = json.NewEncoder(up).Encode(envelope)
		if e != nil {
			trace("bootstrap_backend_write", managedErrorClass(e))
			_ = up.Close()
			return
		}
		trace("bootstrap_backend_write", "none")
		response, e := io.ReadAll(io.LimitReader(up, wlwire.MaxBody+1))
		_ = up.Close()
		switch {
		case len(response) == 0:
			shape = "empty"
		case len(response) > wlwire.MaxBody:
			shape = "oversize"
		case !json.Valid(response):
			shape = "invalid_json"
		default:
			shape = "valid_json"
		}
		trace("bootstrap_backend_read", managedErrorClass(e))
		if e != nil || len(response) > wlwire.MaxBody || !json.Valid(response) {
			switch {
			case len(response) > wlwire.MaxBody:
				trace("bootstrap_backend_response", "oversize")
			case e == nil:
				trace("bootstrap_backend_response", "invalid_json")
			default:
				// The read stage already records the allowlisted error class.
			}
			return
		}
		if e := clientWriteBody(c, id, bytes.TrimSpace(response)); e != nil {
			trace("bootstrap_response_write", managedErrorClass(e))
			return
		}
		trace("bootstrap_response_write", "none")
	}
}

// Also covers a config-only connection that closed before its lease expired,
// and retries an unconfirmed peer removal without depending on client traffic.
func clientTestExpirySweep(wgDev wgDevice) {
	dbMutex.Lock()
	defer dbMutex.Unlock()
	for password, entry := range db.Passwords {
		if entry == nil || entry.ClientTest == nil {
			continue
		}
		g := entry.ClientTest
		if (g.Revoked || !clientTestExpiryActive(entry.ExpiresAt, time.Now().Unix())) && clientTestWorkerCount(g.GrantID) == 0 && !clientTestPeerRemoved[g.GrantID] {
			_ = clientTestApplyRuntime(entry, password, wgDev)
		}
	}
}
func clientTestExpiryLoop(ctx context.Context, wgDev wgDevice) {
	if os.Getenv("WL_TEST_ENABLED") != "1" {
		return
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			clientTestExpirySweep(wgDev)
		}
	}
}

func clientTestWorkerValid(worker, mode string, cap int) bool {
	n, err := strconv.ParseUint(worker, 10, 32)
	if err != nil || strconv.FormatUint(n, 10) != worker || cap <= 0 || n >= uint64(cap) {
		return false
	}
	return (n == 0 && mode == "getconf") || (n > 0 && mode == "data")
}
