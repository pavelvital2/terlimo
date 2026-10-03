package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"wg-turn-client/internal/wlwire"
)

type echoWG struct{ calls atomic.Int32 }

func (w *echoWG) IpcSet(string) error { w.calls.Add(1); return nil }
func (w *echoWG) Close()              { w.calls.Add(1) }

type echoAuthResult struct {
	conn net.Conn
	err  error
}

// The same loopback DTLS/auth seam as managedAuthAttempt; no live WRAP/network/DB.
func echoAuthPair(t *testing.T, identity accessIdentity, g ClientTestGrant, duration time.Duration) (*dtls.Conn, <-chan echoAuthResult, context.CancelFunc) {
	t.Helper()
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := dtls.Listen("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &dtls.Config{Certificates: []tls.Certificate{cert}, ExtendedMasterSecret: dtls.RequireExtendedMasterSecret})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	result := make(chan echoAuthResult, 1)
	wg := &echoWG{}
	t.Cleanup(func() {
		if wg.calls.Load() != 0 {
			t.Error("initial auth modified WG device")
		}
	})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- echoAuthResult{nil, err}
			return
		}
		dc := conn.(*dtls.Conn)
		if err = dc.HandshakeContext(ctx); err != nil {
			dc.Close()
			result <- echoAuthResult{nil, err}
			return
		}
		secured, err := clientTestAuthenticate(ctx, dc, identity, g, wg)
		if err != nil {
			dc.Close()
		}
		result <- echoAuthResult{secured, err}
	}()
	c, err := dtls.Dial("udp4", listener.Addr().(*net.UDPAddr), &dtls.Config{InsecureSkipVerify: true, ExtendedMasterSecret: dtls.RequireExtendedMasterSecret})
	if err != nil {
		cancel()
		listener.Close()
		t.Fatal(err)
	}
	if err = c.HandshakeContext(ctx); err != nil {
		cancel()
		c.Close()
		listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); c.Close(); listener.Close() })
	return c, result, cancel
}
func echoWrite(t *testing.T, c net.Conn, b []byte) {
	t.Helper()
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
}
func echoRead(t *testing.T, c net.Conn) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 8192)
	n, err := c.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	return b[:n]
}
func echoSend(t *testing.T, c net.Conn, id wlwire.ID, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := wlwire.Frames(id, false, b)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		echoWrite(t, c, f)
	}
}
func echoReply(t *testing.T, c net.Conn) map[string]any {
	t.Helper()
	var a wlwire.Assembler
	_, b, err := a.Add(echoRead(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func echoRejected(t *testing.T, result <-chan echoAuthResult, g ClientTestGrant) {
	t.Helper()
	select {
	case r := <-result:
		if r.conn != nil || r.err == nil {
			if r.conn != nil {
				r.conn.Close()
			}
			t.Fatal("unexpected admission")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("auth did not terminate")
	}
	echoNoAdmission(t, g)
}
func echoNoAdmission(t *testing.T, g ClientTestGrant) {
	t.Helper()
	clientTestWorkers.Lock()
	defer clientTestWorkers.Unlock()
	if len(clientTestWorkers.workers[g.GrantID]) != 0 || clientTestWorkers.sessions[g.GrantID] != "" {
		t.Fatal("echo allocated worker/session/config")
	}
}
func echoProof(t *testing.T, c *dtls.Conn, g ClientTestGrant, key *ecdsa.PrivateKey, ch map[string]any) clientAuthBody {
	t.Helper()
	state, _ := c.ConnectionState()
	exporter, err := state.ExportKeyingMaterial("EXPORTER-WL-VPN-POP-1", nil, 32)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := wlwire.Decode(ch["challenge_id"].(string), 16)
	nonce, _ := wlwire.Decode(ch["nonce"].(string), 32)
	p, _ := json.Marshal(clientAuthPayload{Op: "vpn_auth", Node: g.NodeID, Grant: g.GrantID, Registration: g.RegistrationID, Generation: g.Generation, Seq: g.LeaseSeq, Session: "fixture-session-0001", Worker: "0", Mode: "getconf"})
	h := sha256.Sum256(wlwire.Transcript("WL-VPN-POP-1", exporter, cid, nonce, p))
	sig, err := ecdsa.SignASN1(rand.Reader, key, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return clientAuthBody{V: 1, Op: "VPN_AUTH", Challenge: wlwire.Encode(cid), Payload: wlwire.Encode(p), Proof: wlwire.Encode(sig)}
}

func TestManagedInitialEchoA(t *testing.T) {
	t.Run("echo_close", func(t *testing.T) {
		i, g, _ := managedFixture(t)
		c, result, _ := echoAuthPair(t, i, g, 4*time.Second)
		echoWrite(t, c, []byte{0xff})
		if !bytes.Equal(echoRead(t, c), []byte{0xff}) {
			t.Fatal("wrong echo")
		}
		echoNoAdmission(t, g)
		c.Close()
		echoRejected(t, result, g)
	})
	t.Run("echo_begin_complete_auth", func(t *testing.T) {
		i, g, key := managedFixture(t)
		c, result, _ := echoAuthPair(t, i, g, 4*time.Second)
		echoWrite(t, c, []byte{0xff})
		if !bytes.Equal(echoRead(t, c), []byte{0xff}) {
			t.Fatal("wrong echo")
		}
		echoNoAdmission(t, g)
		echoSend(t, c, wlwire.ID{1}, clientAuthBody{V: 1, Op: "VPN_AUTH_BEGIN"})
		ch := echoReply(t, c)
		if ch["op"] != "VPN_CHALLENGE" {
			t.Fatal(ch)
		}
		echoNoAdmission(t, g)
		echoSend(t, c, wlwire.ID{2}, echoProof(t, c, g, key, ch))
		if r := echoReply(t, c); r["op"] != "VPN_AUTH_OK" {
			t.Fatal(r)
		}
		select {
		case r := <-result:
			if r.err != nil || r.conn == nil {
				t.Fatal(r.err)
			}
			r.conn.Close()
		case <-time.After(2 * time.Second):
			t.Fatal("valid auth stalled")
		}
		echoNoAdmission(t, g)
	})
}
func TestManagedInitialEchoB(t *testing.T) {
	for name, b := range map[string][]byte{"prefix": {0xff, 0}, "short": {1, 2}, "wg": {4, 0, 0, 0, 0, 0}, "getconf": []byte("GETCONF:x"), "bad_frame": []byte("WLBSxx")} {
		t.Run(name, func(t *testing.T) {
			i, g, _ := managedFixture(t)
			c, result, _ := echoAuthPair(t, i, g, 4*time.Second)
			echoWrite(t, c, b)
			echoRejected(t, result, g)
			buf := make([]byte, 16)
			c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			if n, _ := c.Read(buf); n > 0 {
				t.Fatal("malformed record received reply")
			}
		})
	}
	t.Run("fragmented_begin_invalid_proof", func(t *testing.T) {
		i, g, _ := managedFixture(t)
		c, result, _ := echoAuthPair(t, i, g, 4*time.Second)
		body := []byte(`{"v":1,"op":"VPN_AUTH_BEGIN"}`)
		body = append(bytes.Repeat([]byte(" "), wlwire.Fragment+10), body...)
		fs, err := wlwire.Frames(wlwire.ID{1}, false, body)
		if err != nil || len(fs) < 2 {
			t.Fatal("fixture not fragmented", err)
		}
		for _, f := range fs {
			echoWrite(t, c, f)
		}
		ch := echoReply(t, c)
		if ch["op"] != "VPN_CHALLENGE" {
			t.Fatal(ch)
		}
		echoSend(t, c, wlwire.ID{2}, clientAuthBody{V: 1, Op: "VPN_AUTH", Challenge: ch["challenge_id"].(string), Payload: "bad", Proof: "bad"})
		echoRejected(t, result, g)
	})
	t.Run("echo_after_fragment_rejected", func(t *testing.T) {
		i, g, _ := managedFixture(t)
		c, result, _ := echoAuthPair(t, i, g, 4*time.Second)
		body := append(bytes.Repeat([]byte(" "), wlwire.Fragment+10), []byte(`{"v":1,"op":"VPN_AUTH_BEGIN"}`)...)
		fs, _ := wlwire.Frames(wlwire.ID{1}, false, body)
		echoWrite(t, c, fs[0])
		echoWrite(t, c, []byte{0xff})
		echoRejected(t, result, g)
	})
	// Assert bytes/ID pass through the real assembler unchanged, with or without echo.
	t.Run("record_preservation", func(t *testing.T) {
		i, g, _ := managedFixture(t)
		body := append(bytes.Repeat([]byte(" "), wlwire.Fragment+10), []byte(`{"v":1,"op":"VPN_AUTH_BEGIN"}`)...)
		id := wlwire.ID{42}
		fs, _ := wlwire.Frames(id, false, body)
		for _, echo := range []bool{false, true} {
			records := fs
			if echo {
				records = append([][]byte{{0xff}}, fs...)
			}
			c := &scriptConn{reads: records}
			gotID, got, err := clientReadInitialBody(context.Background(), c, i, g)
			if err != nil || gotID != id || !bytes.Equal(got, body) {
				t.Fatal("changed initial bytes", err)
			}
		}
	})
}
func TestManagedInitialEchoC(t *testing.T) {
	cases := []string{"missing", "expired", "deactivated", "revoked", "node", "grant", "registration", "generation", "seq", "invalid", "main", "service", "bootstrap"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			i, g, _ := managedFixture(t)
			if name == "invalid" {
				i.id = ""
			}
			i.isMain = name == "main"
			i.isService = name == "service"
			c, result, _ := echoAuthPair(t, i, g, 4*time.Second)
			dbMutex.Lock()
			e := db.Passwords[i.password]
			switch name {
			case "missing":
				delete(db.Passwords, i.password)
			case "expired":
				e.ExpiresAt = time.Now().Add(-time.Second).Unix()
			case "deactivated":
				e.IsDeactivated = true
			case "revoked":
				e.ClientTest.Revoked = true
			case "node":
				e.ClientTest.NodeID = "different"
			case "grant":
				e.ClientTest.GrantID = "different"
			case "registration":
				e.ClientTest.RegistrationID = "different"
			case "generation":
				e.ClientTest.Generation = "different"
			case "seq":
				e.ClientTest.LeaseSeq = "different"
			case "bootstrap":
				db.ClientBootstrap = map[string]ClientTestBootstrap{"fixture": {Secret: i.password}}
			}
			dbMutex.Unlock()
			echoWrite(t, c, []byte{0xff})
			echoRejected(t, result, g)
			buf := make([]byte, 16)
			c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			if n, _ := c.Read(buf); n > 0 {
				t.Fatal("inactive identity echoed")
			}
		})
	}
	t.Run("second_echo", func(t *testing.T) {
		i, g, _ := managedFixture(t)
		c, result, _ := echoAuthPair(t, i, g, 4*time.Second)
		echoWrite(t, c, []byte{0xff})
		echoRead(t, c)
		echoWrite(t, c, []byte{0xff})
		echoRejected(t, result, g)
	})
	t.Run("auth_retry_does_not_reopen_echo", func(t *testing.T) {
		i, g, key := managedFixture(t)
		c, result, _ := echoAuthPair(t, i, g, 4*time.Second)
		echoWrite(t, c, []byte{0xff})
		echoRead(t, c)
		echoSend(t, c, wlwire.ID{1}, clientAuthBody{V: 1, Op: "VPN_AUTH_BEGIN"})
		ch := echoReply(t, c)
		proof := echoProof(t, c, g, key, ch)
		dbMutex.Lock()
		db.Passwords[i.password].ClientTest.LeaseSeq = "2"
		dbMutex.Unlock()
		echoSend(t, c, wlwire.ID{2}, proof)
		if r := echoReply(t, c); r["code"] != "LEASE_CONFLICT" {
			t.Fatal(r)
		}
		echoWrite(t, c, []byte{0xff})
		echoRejected(t, result, g)
	})
	t.Run("deadline_after_echo", func(t *testing.T) {
		i, g, _ := managedFixture(t)
		c, result, _ := echoAuthPair(t, i, g, 400*time.Millisecond)
		echoWrite(t, c, []byte{0xff})
		echoRead(t, c)
		echoRejected(t, result, g)
	})
	t.Run("cancel_after_echo", func(t *testing.T) {
		i, g, _ := managedFixture(t)
		c, result, cancel := echoAuthPair(t, i, g, 4*time.Second)
		echoWrite(t, c, []byte{0xff})
		echoRead(t, c)
		cancel()
		echoRejected(t, result, g)
	})
	t.Run("absolute_deadlines", func(t *testing.T) {
		i, g, _ := managedFixture(t)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second))
		defer cancel()
		until, _ := ctx.Deadline()
		c := &scriptConn{reads: [][]byte{{0xff}}}
		_, _, err := clientReadInitialBody(ctx, c, i, g)
		if err != io.EOF {
			t.Fatal(err)
		}
		if len(c.readDeadlines) != 2 || !c.readDeadlines[0].Equal(until) || !c.readDeadlines[1].IsZero() {
			t.Fatal("read deadline refreshed")
		}
		if len(c.writeDeadlines) != 2 || !c.writeDeadlines[0].Equal(until) {
			t.Fatal("write escaped initial deadline")
		}
		db.Passwords[i.password].ExpiresAt = time.Now().Add(2 * time.Second).Unix()
		s := &scriptConn{}
		clientReadInitialBody(context.Background(), s, i, g)
		if !s.readDeadlines[0].Equal(time.Unix(db.Passwords[i.password].ExpiresAt, 0)) {
			t.Fatal("lease deadline ignored")
		}
	})
}
