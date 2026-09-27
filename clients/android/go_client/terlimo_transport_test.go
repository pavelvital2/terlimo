package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cbeuw/connutil"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
)

func TestManagedPinVerifier(t *testing.T) {
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	pin := base64.RawURLEncoding.EncodeToString(digest[:])
	verify, err := managedPinVerifier(pin)
	if err != nil {
		t.Fatal(err)
	}
	if err = verify(cert.Certificate, nil); err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][][]byte{nil, {{1, 2, 3}}} {
		if err = verify(raw, nil); err == nil || err.Error() != "TRUST_FAILED" {
			t.Fatal("missing/malformed cert accepted")
		}
	}
	other, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	if verify(other.Certificate, nil) == nil {
		t.Fatal("wrong key accepted")
	}
	for _, bad := range []string{"", pin + "=", "abc", strings.Repeat("!", 43)} {
		if _, err = managedPinVerifier(bad); err == nil {
			t.Fatal("bad pin accepted")
		}
	}
}

// Entire handshake uses an in-memory packet pipe: no sockets, live peer or VPN.
func TestManagedPinInDTLSHandshake(t *testing.T) {
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := x509.ParseCertificate(cert.Certificate[0])
	digest := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	for _, matching := range []bool{true, false} {
		t.Run(map[bool]string{true: "match", false: "mismatch"}[matching], func(t *testing.T) {
			pinDigest := digest
			if !matching {
				pinDigest[0] ^= 1
			}
			verify, _ := managedPinVerifier(base64.RawURLEncoding.EncodeToString(pinDigest[:]))
			a, b := connutil.AsyncPacketPipe()
			defer a.Close()
			defer b.Close()
			peer := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 443}
			server, err := dtls.Server(a, peer, &dtls.Config{Certificates: []tls.Certificate{cert}, ExtendedMasterSecret: dtls.RequireExtendedMasterSecret, LoggerFactory: &NullLoggerFactory{}})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			client, err := dtls.Client(b, peer, &dtls.Config{InsecureSkipVerify: true, VerifyPeerCertificate: verify, ExtendedMasterSecret: dtls.RequireExtendedMasterSecret, LoggerFactory: &NullLoggerFactory{}})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- server.HandshakeContext(ctx) }()
			err = client.HandshakeContext(ctx)
			if matching && err != nil {
				t.Fatalf("matching pin handshake failed: %v", err)
			}
			if !matching && err == nil {
				t.Fatal("mismatched pin handshake succeeded")
			}
			cancel()
			<-done
		})
	}
}

func TestManagedAuthGateEveryWorker(t *testing.T) {
	for _, mode := range []string{"getconf", "data"} {
		for _, reject := range []bool{false, true} {
			calls, nativeCalls := 0, 0
			config := &ManagedTransportConfig{Authenticate: func(_ context.Context, _ *dtls.Conn, gotMode string, id int) error {
				calls++
				if gotMode != mode || id != calls {
					t.Fatal("auth worker context mismatch")
				}
				if reject {
					return errors.New("PROOF_INVALID")
				}
				return nil
			}}
			for id := 1; id <= 3; id++ {
				_, err := managedAuthenticateAndRun(context.Background(), nil, config, mode, id, func() (bool, error) { nativeCalls++; return true, nil })
				if reject && (err == nil || err.Error() != "PROOF_INVALID") {
					t.Fatal("proof failure ignored")
				}
				if !reject && err != nil {
					t.Fatal(err)
				}
			}
			if calls != 3 || (reject && nativeCalls != 0) || (!reject && nativeCalls != 3) {
				t.Fatal("auth bypass or auth reuse")
			}
		}
	}
}

func TestManagedAuthCancelAndMissingHookFailClosed(t *testing.T) {
	next := func() (bool, error) { t.Fatal("native path reached"); return false, nil }
	for _, cfg := range []*ManagedTransportConfig{nil, {}} {
		if _, err := managedAuthenticateAndRun(context.Background(), nil, cfg, "data", 1, next); err == nil {
			t.Fatal("missing auth accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &ManagedTransportConfig{Authenticate: func(context.Context, *dtls.Conn, string, int) error { cancel(); return nil }}
	if _, err := managedAuthenticateAndRun(ctx, nil, cfg, "data", 1, next); !errors.Is(err, context.Canceled) {
		t.Fatal("late auth after cancel accepted")
	}
	if managedTransport(WithManagedTransport(context.Background(), nil)) == nil {
		t.Fatal("nil managed config downgraded to legacy")
	}
}

func TestManagedErrorRedaction(t *testing.T) {
	secret := "password|device-id|https://example.test/private?token=secret"
	for _, err := range []error{errors.New(secret), errors.New("TRUST_FAILED: " + secret), errors.New("DENIED:" + secret)} {
		if got := managedSafeError(err).Error(); got != "TRANSPORT_FAILED" {
			t.Fatalf("unsafe error: %q", got)
		}
	}
	if !managedTerminalError(errors.New("TRUST_FAILED")) || !managedTerminalError(errors.New("PROOF_INVALID")) {
		t.Fatal("security failure not terminal")
	}
}

func TestBootstrapRejectsInvalidTrustBeforeCredentials(t *testing.T) {
	_, cleanup, err := DialBootstrapTransport(context.Background(), BootstrapTransportConfig{Pin: "invalid"})
	if cleanup != nil || err == nil || err.Error() != "TRUST_FAILED" {
		t.Fatal("invalid trust reached bootstrap transport")
	}
}
