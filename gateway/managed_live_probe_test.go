package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pion/dtls/v3"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"wg-turn-client/internal/wlwire"
)

// Explicit operator-only TEST measurements; normal test runs never read host state.
func liveProbeTarget(t *testing.T) {
	t.Helper()
	if os.Getenv("WL_LIVE_TEST_PROBE") != "1" {
		t.Skip("explicit TEST operator probe only")
	}
	host, _ := os.Hostname()
	if host != "terlimo.wdtt.vpn" || os.Geteuid() != 0 {
		t.Fatal("TEST target required")
	}
}
func liveProbeJSON(t *testing.T, path string, target any) {
	t.Helper()
	raw, e := os.ReadFile(path)
	if e != nil || json.Unmarshal(raw, target) != nil {
		t.Fatal("protected input unavailable")
	}
}
func liveProbeDial(t *testing.T, password, pin string) *dtls.Conn {
	t.Helper()
	key, err := deriveWrapKey(password)
	if err != nil {
		t.Fatal("wrap derivation")
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal("local UDP unavailable")
	}
	wrapped := &wrapPacketConn{inner: udp, key: key}
	atomic.StoreInt32(&wrapped.selected, 1)
	verified := false
	c, err := dtls.Client(wrapped, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 56000}, &dtls.Config{
		InsecureSkipVerify:   true, // Exact SPKI pin below is the trust check.
		ExtendedMasterSecret: dtls.RequireExtendedMasterSecret,
		CipherSuites:         []dtls.CipherSuiteID{dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) != 1 {
				return errors.New("certificate shape")
			}
			leaf, e := x509.ParseCertificate(rawCerts[0])
			if e != nil {
				return errors.New("certificate parse")
			}
			hash := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			if base64.RawURLEncoding.EncodeToString(hash[:]) != pin {
				return errors.New("pin mismatch")
			}
			verified = true
			return nil
		},
	})
	if err != nil {
		wrapped.Close()
		t.Fatal("DTLS client")
	}
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c.HandshakeContext(ctx) != nil || !verified {
		t.Fatal("live pinned WRAP/DTLS handshake failed")
	}
	return c
}
func liveProbeDenied(t *testing.T, c *dtls.Conn) {
	t.Helper()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.Write([]byte("READY"))
	b := make([]byte, 256)
	n, err := c.Read(b)
	if n != 0 || err == nil {
		t.Fatal("forbidden VPN relay responded")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("timeout is not evidence of explicit relay rejection")
	}
}
func TestManagedLivePinAndLegacyRelayDenied(t *testing.T) {
	liveProbeTarget(t)
	var state struct {
		MainPassword string `json:"main_password"`
	}
	liveProbeJSON(t, "/etc/wdtt/passwords.json", &state)
	var trust struct {
		Pin string `json:"dtls_spki_sha256"`
	}
	liveProbeJSON(t, "/var/backups/terlimo-managed-20260910/public-trust.json", &trust)
	if state.MainPassword == "" || trust.Pin == "" {
		t.Fatal("protected input shape")
	}
	c := liveProbeDial(t, state.MainPassword, trust.Pin)
	state.MainPassword = ""
	liveProbeDenied(t, c)
}
func TestManagedLiveSignedBootstrapChallengeAndRelayDenied(t *testing.T) {
	liveProbeTarget(t)
	raw, e := os.ReadFile("/var/backups/terlimo-managed-20260910/subscription-link.txt")
	if e != nil {
		t.Fatal("protected link unavailable")
	}
	link := strings.TrimSpace(string(raw))
	prefix := "whitelists://subscription?v=1#"
	if !strings.HasPrefix(link, prefix) {
		t.Fatal("link shape")
	}
	envelope, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(link, prefix))
	if e != nil {
		t.Fatal("envelope encoding")
	}
	var env struct {
		Kid       string `json:"kid"`
		Payload   string `json:"payload_b64"`
		Signature string `json:"signature_b64"`
	}
	if json.Unmarshal(envelope, &env) != nil {
		t.Fatal("envelope shape")
	}
	var trust struct {
		Kid  string `json:"issuer_kid"`
		SPKI string `json:"issuer_spki_b64url"`
		Pin  string `json:"dtls_spki_sha256"`
	}
	liveProbeJSON(t, "/var/backups/terlimo-managed-20260910/public-trust.json", &trust)
	payload, pe := base64.RawURLEncoding.DecodeString(env.Payload)
	sig, se := base64.RawURLEncoding.DecodeString(env.Signature)
	spki, ke := base64.RawURLEncoding.DecodeString(trust.SPKI)
	if pe != nil || se != nil || ke != nil || env.Kid != trust.Kid {
		t.Fatal("signed link encoding")
	}
	key, ke := x509.ParsePKIXPublicKey(spki)
	pub, ok := key.(*ecdsa.PublicKey)
	hash := sha256.Sum256(append([]byte("WL-LINK-1\x00"), payload...))
	if ke != nil || !ok || !ecdsa.VerifyASN1(pub, hash[:], sig) {
		t.Fatal("issuer signature")
	}
	var p struct {
		Env       string `json:"env"`
		Secret    string `json:"bootstrap_secret"`
		Bootstrap []struct {
			Peer string `json:"peer_ip"`
			Pin  string `json:"dtls_spki_sha256"`
		} `json:"bootstrap"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Env != "test" || p.Secret == "" || len(p.Bootstrap) != 1 || p.Bootstrap[0].Peer != "193.5.251.217" || p.Bootstrap[0].Pin != trust.Pin {
		t.Fatal("bootstrap target")
	}
	c := liveProbeDial(t, p.Secret, trust.Pin)
	p.Secret = ""
	id := wlwire.ID{9, 10, 26, 1}
	body := []byte(`{"v":1,"op":"challenge"}`)
	frames, e := wlwire.Frames(id, false, body)
	if e != nil {
		t.Fatal("frame encoding")
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	for _, f := range frames {
		if _, e = c.Write(f); e != nil {
			t.Fatal("bootstrap send")
		}
	}
	var assembler wlwire.Assembler
	for {
		buf := make([]byte, 32+wlwire.Fragment+1)
		n, e := c.Read(buf)
		if e != nil {
			t.Fatal("bootstrap response unavailable")
		}
		if n < 6 || buf[5] != 1 {
			t.Fatal("response framing")
		}
		rid, b, e := assembler.Add(buf[:n], time.Now())
		if e != nil || rid != id {
			t.Fatal("response correlation")
		}
		if b == nil {
			continue
		}
		var reply struct {
			V      int    `json:"v"`
			Op     string `json:"op"`
			Status string `json:"status"`
		}
		if json.Unmarshal(b, &reply) != nil || reply.V != 1 || reply.Op != "challenge" || reply.Status != "ok" {
			t.Fatal("bootstrap gateway response")
		}
		break
	}
	liveProbeDenied(t, c)
}
