package wlbs

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type vector struct {
	Kind          string   `json:"kind"`
	PublicKeySPKI string   `json:"public_key_spki"`
	RequestIDHex  string   `json:"request_id_hex"`
	ChallengeID   string   `json:"challenge_id"`
	Nonce         string   `json:"nonce"`
	Exporter      string   `json:"synthetic_exporter_hex"`
	PayloadB64    string   `json:"payload_b64"`
	TranscriptHex string   `json:"transcript_hex"`
	SHA           string   `json:"sha256_transcript"`
	ProofB64      string   `json:"proof_b64"`
	FramesHex     []string `json:"frames_hex"`
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, e := hex.DecodeString(s)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, e := DecodeBinary(s, -1)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func wireCode(t *testing.T, e error, want string) {
	t.Helper()
	var got *Error
	if !errors.As(e, &got) || got.Code != want {
		t.Fatalf("error=%v want=%s", e, want)
	}
}
func TestCanonicalVectors(t *testing.T) {
	for _, fixture := range []struct{ file, sha string }{
		{"wl_wire_vectors.json", "c8157e92c9c62e6781a2c8cb48fa020bb3f43f81414ce0890c96a68968a442dd"},
		{"wl_wire_vectors_v2.json", "0852d628bce188d61da7c49c40ca21653909547579bc8f7225724c0dab8ec832"},
	} {
		t.Run(fixture.file, func(t *testing.T) { checkCanonicalVectors(t, fixture.file, fixture.sha) })
	}
}

func checkCanonicalVectors(t *testing.T, file, expectedSHA string) {
	t.Helper()
	raw, e := os.ReadFile("../../docs/fixtures/" + file)
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(raw)
	if hex.EncodeToString(h[:]) != expectedSHA {
		t.Fatal("fixture bytes changed")
	}
	var f struct {
		Vectors []vector `json:"vectors"`
		Link    vector   `json:"link"`
	}
	if e = StrictJSON(raw, &f); e != nil {
		t.Fatal(e)
	}
	for _, v := range f.Vectors {
		t.Run(v.Kind, func(t *testing.T) {
			var id ID
			copy(id[:], unhex(t, v.RequestIDHex))
			cid, nonce, payload := unb64(t, v.ChallengeID), unb64(t, v.Nonce), unb64(t, v.PayloadB64)
			var transcript []byte
			var e error
			if v.Kind == "bootstrap" {
				transcript, e = BootstrapTranscript(id, cid, nonce, payload)
			} else {
				transcript, e = VPNTranscript(unhex(t, v.Exporter), cid, nonce, payload)
				var identity VPNIdentity
				if err := StrictJSON(payload, &identity); err != nil {
					t.Fatal(err)
				}
				// Both historical crypto vectors deliberately use schema-invalid
				// worker-0. This does not make that worker ID acceptable on wire.
				wireCode(t, identity.Validate(), "BAD_MESSAGE")
				if file == "wl_wire_vectors_v2.json" {
					identity.WorkerID = "0"
					if err := identity.Validate(); err != nil {
						t.Fatal("v2 session must satisfy native schema after correcting crypto-only worker ID")
					}
				}
			}
			if e != nil || !bytes.Equal(transcript, unhex(t, v.TranscriptHex)) {
				t.Fatal("transcript mismatch")
			}
			h := sha256.Sum256(transcript)
			if hex.EncodeToString(h[:]) != v.SHA {
				t.Fatal("hash mismatch")
			}
			pub, proof := unb64(t, v.PublicKeySPKI), unb64(t, v.ProofB64)
			if e = VerifyTranscript(pub, transcript, proof); e != nil {
				t.Fatal(e)
			}
			wireCode(t, VerifyTranscript(pub, h[:], proof), "PROOF_INVALID")
			changed := append([]byte(nil), transcript...)
			changed[15] ^= 1
			wireCode(t, VerifyTranscript(pub, changed, proof), "PROOF_INVALID")
			op := "register"
			if v.Kind == "vpn" {
				op = "VPN_AUTH"
			}
			body, _ := json.Marshal(ProofEnvelope{1, op, v.ChallengeID, v.PayloadB64, v.ProofB64})
			frames, e := Frames(id, false, body)
			if e != nil || len(frames) != len(v.FramesHex) {
				t.Fatal("frames")
			}
			for i, frame := range frames {
				if !bytes.Equal(frame, unhex(t, v.FramesHex[i])) {
					t.Fatal("frame bytes differ")
				}
			}
		})
	}
	v := f.Link
	payload := unb64(t, v.PayloadB64)
	if !bytes.Equal(LinkTranscript(payload), unhex(t, v.TranscriptHex)) {
		t.Fatal("link transcript")
	}
	pub := unb64(t, v.PublicKeySPKI)
	if e := VerifyTranscript(pub, LinkTranscript(payload), unb64(t, v.ProofB64)); e != nil {
		t.Fatal(e)
	}
	env, _ := json.Marshal(LinkEnvelope{"fixture-only", v.PayloadB64, v.ProofB64})
	_, e = VerifyLink("whitelists://subscription?v=1#"+EncodeBinary(env), map[string][]byte{"fixture-only": pub}, "test", time.Now())
	wireCode(t, e, "TRUST_FAILED")
}
func TestFramingBoundsReorderDuplicate(t *testing.T) {
	id, _ := NewID()
	for _, size := range []int{1, 1024, 1025, MaxBody} {
		body := bytes.Repeat([]byte{'a'}, size)
		frames, e := Frames(id, false, body)
		if e != nil {
			t.Fatal(e)
		}
		a := NewReassembler(id, false)
		now := time.Now()
		for i := len(frames) - 1; i >= 0; i-- {
			got, done, e := a.Add(frames[i], now)
			if e != nil {
				t.Fatal(e)
			}
			if i > 0 && done {
				t.Fatal("premature")
			}
			if i == 0 && (!done || !bytes.Equal(got, body)) {
				t.Fatal("reassembly")
			}
			if _, _, e = a.Add(frames[i], now); e != nil {
				t.Fatal("identical duplicate")
			}
		}
	}
	for _, size := range []int{0, MaxBody + 1} {
		_, e := Frames(id, false, make([]byte, size))
		wireCode(t, e, "BAD_MESSAGE")
	}
}
func TestFragmentRejects(t *testing.T) {
	id, _ := NewID()
	frames, _ := Frames(id, true, bytes.Repeat([]byte{'x'}, 1025))
	for _, mutate := range []func([]byte) []byte{func(b []byte) []byte { b[4] = 2; return b }, func(b []byte) []byte { b[5] = 2; return b }, func(b []byte) []byte { b[7] = 31; return b }, func(b []byte) []byte { b[8] ^= 1; return b }, func(b []byte) []byte { b[31] = 1; return b }, func(b []byte) []byte { return append(b, 0) }} {
		a := NewReassembler(id, true)
		_, _, e := a.Add(mutate(append([]byte(nil), frames[0]...)), time.Now())
		wireCode(t, e, "BAD_MESSAGE")
	}
	a := NewReassembler(id, true)
	now := time.Now()
	_, _, _ = a.Add(frames[0], now)
	bad := append([]byte(nil), frames[0]...)
	bad[32] ^= 1
	_, _, e := a.Add(bad, now)
	wireCode(t, e, "BAD_MESSAGE")
	a = NewReassembler(id, true)
	_, _, _ = a.Add(frames[0], now)
	_, _, _ = a.Add(frames[0], now.Add(9*time.Second))
	_, _, e = a.Add(frames[1], now.Add(10*time.Second))
	wireCode(t, e, "TRANSPORT_TIMEOUT")
}
func TestStrictJSON(t *testing.T) {
	for _, s := range []string{`{"v":1,"v":1}`, `{"a":{"x":1,"x":2}}`, `{"x":NaN}`, `{} {}`, "{\"x\":\"\xff\"}", strings.Repeat("[", 34) + strings.Repeat("]", 34)} {
		var out any
		wireCode(t, StrictJSON([]byte(s), &out), "BAD_MESSAGE")
	}
	var out any
	if e := StrictJSON([]byte(`{"n":9007199254740993,"a":[true,null]}`), &out); e != nil {
		t.Fatal(e)
	}
}
func TestDecimal(t *testing.T) {
	cmp, e := CompareDecimal("9007199254740993", "9007199254740992")
	if e != nil || cmp != 1 {
		t.Fatal("precision")
	}
	for _, s := range []string{"", "01", "1.0", "-1", "+1", "1e3"} {
		_, e := CompareDecimal(s, "0")
		wireCode(t, e, "BAD_MESSAGE")
	}
}

func localKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	pub, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		t.Fatal(e)
	}
	return key, pub
}
func sign(t *testing.T, key *ecdsa.PrivateKey, transcript []byte) []byte {
	t.Helper()
	h := sha256.Sum256(transcript)
	b, e := ecdsa.SignASN1(rand.Reader, key, h[:])
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestPermanentLinkAndPin(t *testing.T) {
	key, pub := localKey(t)
	now := time.Now().UTC().Truncate(time.Second)
	pinHash := sha256.Sum256(pub)
	pin := EncodeBinary(pinHash[:])
	link := Link{V: 1, Env: "test", SubscriptionRef: "fixture-only", CredentialID: "fixture-only", BootstrapSecret: "synthetic-not-a-credential", Bootstrap: []Endpoint{{"test-1", "192.0.2.1", 56000, pin}}, VKHashes: []string{"synthetic"}, IssuedAt: now.Format(time.RFC3339)}
	raw, _ := json.Marshal(link)
	envelope, _ := json.Marshal(LinkEnvelope{"fixture-only", EncodeBinary(raw), EncodeBinary(sign(t, key, LinkTranscript(raw)))})
	uri := "whitelists://subscription?v=1#" + EncodeBinary(envelope)
	if _, e := VerifyLink(uri, map[string][]byte{"fixture-only": pub}, "test", now); e != nil {
		t.Fatal(e)
	}
	_, e := VerifyLink(uri, nil, "test", now)
	wireCode(t, e, "TRUST_FAILED")
	_, e = VerifyLink(uri, map[string][]byte{"fixture-only": pub}, "prod", now)
	wireCode(t, e, "TRUST_FAILED")
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now, NotAfter: now.Add(time.Hour)}
	cert, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	if e = VerifySPKIPin(cert, pin); e != nil {
		t.Fatal(e)
	}
	wireCode(t, VerifySPKIPin(cert, EncodeBinary(make([]byte, 32))), "TRUST_FAILED")
}

func testCatalog() Catalog {
	now := time.Now().UTC().Truncate(time.Second)
	return Catalog{V: 1, Status: "ok", SubscriptionRef: "fixture-only", RegistrationID: "registration", SubscriptionStatus: "active", SubscriptionExpiresAt: now.Add(time.Hour).Format(time.RFC3339), SlotsLimit: 2, SlotsUsed: 1, Revision: "9007199254740993", IssuedAt: now.Format(time.RFC3339), RefreshAfter: now.Add(5 * time.Minute).Format(time.RFC3339), CatalogExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339), Nodes: []Node{{Endpoint: Endpoint{"test-1", "192.0.2.1", 56000, EncodeBinary(make([]byte, 32))}, Name: "TEST", CountryCode: "ZZ", WGPort: 51820, Protocol: "wdtt-v17", AuthMode: "installation-pop-v1", MaxWorkers: 1, Access: Access{GrantID: "grant", DeviceID: "registration", Password: "synthetic", VKHashes: []string{"synthetic"}, ExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339), Generation: "1", LeaseSeq: "1"}}}}
}
func TestCatalogAtomicRevisionAndExpiry(t *testing.T) {
	c := testCatalog()
	store := &CatalogStore{}
	apply := func(c *Catalog) error { return store.Apply(c, "fixture-only", "registration", time.Now()) }
	if e := apply(&c); e != nil {
		t.Fatal(e)
	}
	lower := *cloneCatalog(&c)
	lower.Revision = "9007199254740992"
	wireCode(t, apply(&lower), "STALE_CATALOG")
	conflict := *cloneCatalog(&c)
	conflict.Nodes[0].Access.LeaseSeq = "2"
	wireCode(t, apply(&conflict), "REVISION_CONFLICT")
	partial := *cloneCatalog(&c)
	partial.Revision = "9007199254740994"
	partial.Nodes = nil
	wireCode(t, apply(&partial), "BAD_CATALOG")
	before := store.Snapshot()
	if e := store.NotModified(c.Revision, time.Now()); e != nil {
		t.Fatal(e)
	}
	after := store.Snapshot()
	if before.CatalogExpiresAt != after.CatalogExpiresAt || before.Nodes[0].Access.ExpiresAt != after.Nodes[0].Access.ExpiresAt {
		t.Fatal("not_modified renewed lease")
	}
	expiry, _ := UTC(c.CatalogExpiresAt)
	wireCode(t, store.NotModified(c.Revision, expiry), "CATALOG_EXPIRED")
	if store.Snapshot().Revision != c.Revision {
		t.Fatal("cache corrupted")
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "synthetic timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

type recordConn struct {
	mu        sync.Mutex
	records   [][]byte
	writes    [][]byte
	closed    bool
	responder func([]byte) [][]byte
	readError error
	block     chan struct{}
	once      sync.Once
}

func (c *recordConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if len(c.records) > 0 {
		p := c.records[0]
		c.records = c.records[1:]
		c.mu.Unlock()
		return copy(b, p), nil
	}
	block, err, closed := c.block, c.readError, c.closed
	c.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	if block != nil {
		<-block
		return 0, net.ErrClosed
	}
	if err != nil {
		return 0, err
	}
	return 0, io.EOF
}
func (c *recordConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	p := append([]byte(nil), b...)
	c.writes = append(c.writes, p)
	if c.responder != nil {
		c.records = append(c.records, c.responder(p)...)
	}
	return len(b), nil
}
func (c *recordConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	if c.block != nil {
		c.once.Do(func() { close(c.block) })
	}
	return nil
}
func (c *recordConn) LocalAddr() net.Addr              { return nil }
func (c *recordConn) RemoteAddr() net.Addr             { return nil }
func (c *recordConn) SetDeadline(time.Time) error      { return nil }
func (c *recordConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recordConn) SetWriteDeadline(time.Time) error { return nil }
func TestRPCBudgetAndCancel(t *testing.T) {
	id, _ := NewID()
	c := &recordConn{readError: timeoutError{}}
	r := &RPC{Conn: c, Jitter: func(time.Duration) time.Duration { return 0 }}
	_, e := r.Call(context.Background(), id, []byte(`{"v":1,"op":"challenge"}`))
	wireCode(t, e, "TRANSPORT_TIMEOUT")
	if len(c.writes) != 3 {
		t.Fatalf("sent %d", len(c.writes))
	}
	for _, w := range c.writes {
		if !bytes.Equal(w, c.writes[0]) {
			t.Fatal("retry changed bytes")
		}
	}
	c = &recordConn{block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	r = &RPC{Conn: c}
	done := make(chan error)
	go func() { _, e := r.Call(ctx, id, []byte(`{"v":1}`)); done <- e }()
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel blocked")
	}
}
func TestRPCTypedTerminalError(t *testing.T) {
	id, _ := NewID()
	c := &recordConn{}
	c.responder = func(b []byte) [][]byte {
		frames, _ := Frames(id, true, []byte(`{"v":1,"status":"error","code":"DEVICE_LIMIT_REACHED","retryable":true}`))
		return frames
	}
	r := &RPC{Conn: c}
	_, e := r.Call(context.Background(), id, []byte(`{"v":1}`))
	wireCode(t, e, "DEVICE_LIMIT_REACHED")
	if len(c.writes) != 1 {
		t.Fatal("terminal retried")
	}
}
func TestLeaseExpiryIdleRefreshRevoke(t *testing.T) {
	guard, e := NewLeaseGuard("1", "1", time.Now().Add(30*time.Millisecond))
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error)
	closed := make(chan struct{})
	go func() { done <- guard.Watch(context.Background(), "1", func() { close(closed) }) }()
	wireCode(t, <-done, "LEASE_EXPIRED")
	<-closed
	guard, _ = NewLeaseGuard("1", "1", time.Now().Add(time.Minute))
	wireCode(t, guard.Update("1", "0", time.Now().Add(time.Hour), false), "LEASE_CONFLICT")
	if e := guard.Update("1", "2", time.Now().Add(2*time.Minute), false); e != nil {
		t.Fatal(e)
	}
	if e := guard.Check("1", time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := guard.Update("2", "2", time.Now(), true); e != nil {
		t.Fatal(e)
	}
	wireCode(t, guard.Check("1", time.Now()), "GRANT_REVOKED")
	wireCode(t, guard.Update("2", "3", time.Now().Add(time.Hour), false), "GRANT_REVOKED")
}

func TestVPNAuthFlowAndLeaseConflict(t *testing.T) {
	key, pub := localKey(t)
	identity := VPNIdentity{"test-1", "grant", "registration", "1", "1", "0123456789abcdef0123456789abcdef", "0", "getconf"}
	exporter := bytes.Repeat([]byte{0x41}, 32)
	cid := bytes.Repeat([]byte{0x42}, 16)
	nonce := bytes.Repeat([]byte{0x43}, 32)
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "lease-conflict"}[conflict], func(t *testing.T) {
			c := &recordConn{}
			authCalls, beginCalls := 0, 0
			var beginID ID
			c.responder = func(record []byte) [][]byte {
				var id ID
				copy(id[:], record[8:24])
				var meta ProofEnvelope
				if e := StrictJSON(record[32:], &meta); e != nil {
					t.Fatal(e)
				}
				now := time.Now().UTC().Truncate(time.Second)
				var response any
				switch meta.Op {
				case "VPN_AUTH_BEGIN":
					beginCalls++
					beginID = id
					seq := "1"
					if authCalls > 0 {
						seq = "2"
					}
					response = VPNChallenge{V: 1, Op: "VPN_CHALLENGE", Status: "ok", ChallengeID: EncodeBinary(cid), Nonce: EncodeBinary(nonce), GrantID: identity.GrantID, RegistrationID: identity.RegistrationID, Generation: "1", LeaseSeq: seq, NodeID: identity.NodeID, ChallengeExpiresAt: now.Add(15 * time.Second).Format(time.RFC3339), AccessExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339), ServerTime: now.Format(time.RFC3339)}
				case "VPN_AUTH":
					authCalls++
					if id == beginID {
						t.Fatal("request IDs reused")
					}
					payload := unb64(t, meta.PayloadB64)
					transcript, _ := VPNTranscript(exporter, cid, nonce, payload)
					proof := unb64(t, meta.ProofB64)
					if e := VerifyTranscript(pub, transcript, proof); e != nil {
						t.Fatal(e)
					}
					other := append([]byte(nil), exporter...)
					other[0] ^= 1
					replayed, _ := VPNTranscript(other, cid, nonce, payload)
					wireCode(t, VerifyTranscript(pub, replayed, proof), "PROOF_INVALID")
					if conflict && authCalls == 1 {
						response = struct {
							V      int    `json:"v"`
							Op     string `json:"op"`
							Status string `json:"status"`
							Error
						}{1, "VPN_AUTH_ERROR", "error", Error{Code: "LEASE_CONFLICT"}}
					} else {
						var p struct {
							Op string `json:"op"`
							VPNIdentity
						}
						if e := StrictJSON(payload, &p); e != nil || p.Op != "vpn_auth" {
							t.Fatal("payload")
						}
						response = VPNOK{V: 1, Op: "VPN_AUTH_OK", Status: "ok", ChallengeID: meta.ChallengeID, VPNIdentity: p.VPNIdentity, AccessExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339), ServerTime: now.Format(time.RFC3339)}
					}
				default:
					t.Fatal("unexpected op")
				}
				raw, _ := json.Marshal(response)
				frames, e := Frames(id, true, raw)
				if e != nil {
					t.Fatal(e)
				}
				return frames
			}
			result, e := AuthenticateVPN(context.Background(), c, func(label string, context []byte, length int) ([]byte, error) {
				if label != ExporterLabel || context != nil || length != 32 {
					t.Fatal("exporter arguments")
				}
				return exporter, nil
			}, func(ctx context.Context, tBytes []byte) ([]byte, error) { return sign(t, key, tBytes), nil }, identity)
			if e != nil {
				t.Fatal(e)
			}
			want := 1
			if conflict {
				want = 2
			}
			if authCalls != want || beginCalls != want || result.Generation != "1" || c.closed {
				t.Fatal("auth flow mismatch")
			}
		})
	}
}

// Exact deployed gateway field sets, independent of permissive meta decoding.
func exactBootstrapKeys(raw []byte, keys ...string) bool {
	var fields map[string]json.RawMessage
	if StrictJSON(raw, &fields) != nil || len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	return true
}

func TestDeployedChallengeRejectsOldExtraCredential(t *testing.T) {
	if exactBootstrapKeys([]byte(`{"v":1,"op":"challenge","credential_id":"synthetic"}`), "v", "op") {
		t.Fatal("old challenge extra accepted")
	}
	if !exactBootstrapKeys([]byte(`{"v":1,"op":"challenge"}`), "v", "op") {
		t.Fatal("valid challenge rejected")
	}
}

func TestBootstrapPersistsBeforeSendAndSameIDResume(t *testing.T) {
	key, pub := localKey(t)
	c := &recordConn{}
	persisted := false
	var pending PendingOperation
	var mutationIDs []ID
	cid, nonce := bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 32)
	c.responder = func(record []byte) [][]byte {
		if !persisted {
			t.Fatal("send before durable operation")
		}
		var id ID
		copy(id[:], record[8:24])
		var meta ProofEnvelope
		if e := StrictJSON(record[32:], &meta); e != nil {
			t.Fatal(e)
		}
		var response any
		switch meta.Op {
		case "challenge":
			if !exactBootstrapKeys(record[32:], "v", "op") {
				response = map[string]any{"v": 1, "status": "error", "code": "BAD_MESSAGE", "retryable": false}
				break
			}
			if meta.V != 1 {
				t.Fatal("challenge version type/value")
			}
			now := time.Now().UTC().Truncate(time.Second)
			response = BootstrapChallenge{V: 1, Op: "challenge", Status: "ok", ChallengeID: EncodeBinary(cid), Nonce: EncodeBinary(nonce), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), ServerTime: now.Format(time.RFC3339)}
		case "register":
			if !exactBootstrapKeys(record[32:], "v", "op", "challenge_id", "payload_b64", "proof_b64") {
				t.Fatal("signed envelope keys")
			}
			if _, e := ParseID(meta.ChallengeID); e != nil {
				t.Fatal("challenge ID encoding")
			}
			mutationIDs = append(mutationIDs, id)
			if meta.PayloadB64 != pending.PayloadB64 {
				t.Fatal("mutation payload changed")
			}
			payload := unb64(t, meta.PayloadB64)
			if !exactBootstrapKeys(payload, "op", "credential_id", "public_key_spki", "installation_id", "os") {
				t.Fatal("register payload keys")
			}
			var registration RegisterPayload
			if StrictJSON(payload, &registration) != nil || registration.Op != meta.Op || registration.OS != "android" || registration.CredentialID != "fixture-only" {
				t.Fatal("register payload types/binding")
			}
			if parsed, e := ParseID(pending.RequestID); e != nil || parsed != id {
				t.Fatal("pending canonical ID")
			}
			tr, _ := BootstrapTranscript(id, cid, nonce, payload)
			if e := VerifyTranscript(pub, tr, unb64(t, meta.ProofB64)); e != nil {
				t.Fatal(e)
			}
			response = map[string]any{"v": 1, "status": "ok"}
		default:
			t.Fatal("unexpected op")
		}
		body, _ := json.Marshal(response)
		frames, _ := Frames(id, true, body)
		return frames
	}
	client := &BootstrapClient{RPC: &RPC{Conn: c}, Signer: func(ctx context.Context, tr []byte) ([]byte, error) { return sign(t, key, tr), nil }, CredentialID: "fixture-only", PublicKeySPKI: pub, Persist: func(ctx context.Context, p PendingOperation) error { persisted = true; pending = p; return nil }}
	_, p, e := client.Register(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = client.Resume(context.Background(), *p); e != nil {
		t.Fatal(e)
	}
	if len(mutationIDs) != 2 || mutationIDs[0] != mutationIDs[1] {
		t.Fatal("resume changed mutation id")
	}
	client.Persist = nil
	_, _, e = client.Register(context.Background())
	wireCode(t, e, "PERSIST_REQUIRED")
}

func TestAgreedSchemaFixtures(t *testing.T) {
	raw, e := os.ReadFile("../../docs/fixtures/wl_schema_vectors.json")
	if e != nil {
		t.Fatal(e)
	}
	var f struct {
		VPNPayload VPNIdentity        `json:"vpn_payload"`
		Catalog    Catalog            `json:"catalog"`
		Register   CatalogResponse    `json:"register_success"`
		Refresh    CatalogResponse    `json:"refresh_access_success"`
		Complete   OperationStatus    `json:"operation_status_complete"`
		Pending    OperationStatus    `json:"operation_status_pending"`
		Failed     OperationStatus    `json:"operation_status_failed"`
		Unknown    OperationStatus    `json:"operation_status_unknown"`
		Challenge  BootstrapChallenge `json:"bootstrap_challenge"`
	}
	if e := StrictJSON(raw, &f); e != nil {
		t.Fatal(e)
	}
	if e := f.VPNPayload.ValidateWorker(1); e != nil {
		t.Fatal(e)
	}
	now, _ := UTC("2026-09-10T12:00:00Z")
	if e := f.Catalog.Validate("fixture-only", f.Catalog.RegistrationID, now); e != nil {
		t.Fatal(e)
	}
	for _, r := range []CatalogResponse{f.Register, f.Refresh} {
		if e := r.Validate(); e != nil {
			t.Fatal(e)
		}
		if e := r.Catalog.Validate("fixture-only", f.Catalog.RegistrationID, now); e != nil {
			t.Fatal(e)
		}
	}
	for _, s := range []OperationStatus{f.Complete, f.Pending, f.Failed, f.Unknown} {
		if e := s.Validate(); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := UTC(f.Challenge.ServerTime); e != nil {
		t.Fatal(e)
	}
	for _, worker := range []string{"worker-0", "00", "-1", "1"} {
		id := f.VPNPayload
		id.WorkerID = worker
		wireCode(t, id.ValidateWorker(1), "BAD_MESSAGE")
	}
	id := f.VPNPayload
	id.WorkerID = "1"
	id.Mode = "data"
	if e := id.ValidateWorker(2); e != nil {
		t.Fatal(e)
	}
	wireCode(t, id.ValidateWorker(1), "BAD_MESSAGE")
	wireCode(t, (&OperationStatus{V: 1, Status: "complete"}).Validate(), "BAD_CATALOG")
	wireCode(t, (&CatalogResponse{V: 1, Status: "ok"}).Validate(), "BAD_CATALOG")
}

func TestVPNTransportSessionCanonicalBoundaries(t *testing.T) {
	id := VPNIdentity{"test-1", "grant", "registration", "1", "1", "0123456789abcdef", "0", "getconf"}
	for _, session := range []string{strings.Repeat("a", 16), strings.Repeat("Z", 64), "Ab09-_Ab09-_Ab09"} {
		id.TransportSession = session
		if e := id.Validate(); e != nil {
			t.Fatalf("valid session rejected: length %d", len(session))
		}
	}
	for _, session := range []string{"", strings.Repeat("a", 15), strings.Repeat("a", 65), " 0123456789abcdef", "0123456789abcdef ", "0123456789abcde/", "0123456789abcde\x00", "0123456789abcdeя", "0123456789abcde."} {
		id.TransportSession = session
		wireCode(t, id.Validate(), "BAD_MESSAGE")
	}
}
