package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"wg-turn-client/internal/wlwire"
)

func managedAuthAttempt(t *testing.T, ctx context.Context, listener net.Listener, identity accessIdentity, grant ClientTestGrant, key *ecdsa.PrivateKey, session, worker string) (*dtls.Conn, net.Conn, error, string) {
	t.Helper()
	serverResult := make(chan struct {
		conn net.Conn
		err  error
	}, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			dtlsConn := conn.(*dtls.Conn)
			err = dtlsConn.HandshakeContext(ctx)
			if err == nil {
				var secured net.Conn
				secured, err = clientTestAuthenticate(ctx, dtlsConn, identity, grant, nil)
				serverResult <- struct {
					conn net.Conn
					err  error
				}{secured, err}
				return
			}
		}
		serverResult <- struct {
			conn net.Conn
			err  error
		}{nil, err}
	}()

	client, err := dtls.Dial("udp4", listener.Addr().(*net.UDPAddr), &dtls.Config{InsecureSkipVerify: true, ExtendedMasterSecret: dtls.RequireExtendedMasterSecret})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.HandshakeContext(ctx); err != nil {
		client.Close()
		t.Fatal(err)
	}
	send := func(id wlwire.ID, value any) {
		body, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		frames, frameErr := wlwire.Frames(id, false, body)
		if frameErr != nil {
			t.Fatal(frameErr)
		}
		for _, frame := range frames {
			if _, writeErr := client.Write(frame); writeErr != nil {
				t.Fatal(writeErr)
			}
		}
	}
	recv := func() map[string]any {
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 2048)
		n, readErr := client.Read(buf)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var assembler wlwire.Assembler
		_, body, assembleErr := assembler.Add(buf[:n], time.Now())
		if assembleErr != nil {
			t.Fatal(assembleErr)
		}
		var value map[string]any
		if unmarshalErr := json.Unmarshal(body, &value); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		return value
	}

	requestID := wlwire.ID{1}
	send(requestID, map[string]any{"v": 1, "op": "VPN_AUTH_BEGIN"})
	challenge := recv()
	state, ok := client.ConnectionState()
	if !ok {
		t.Fatal("missing DTLS connection state")
	}
	exporter, err := state.ExportKeyingMaterial("EXPORTER-WL-VPN-POP-1", nil, 32)
	if err != nil {
		t.Fatal(err)
	}
	challengeID, _ := wlwire.Decode(challenge["challenge_id"].(string), 16)
	nonce, _ := wlwire.Decode(challenge["nonce"].(string), 32)
	payload, _ := json.Marshal(clientAuthPayload{Op: "vpn_auth", Node: grant.NodeID, Grant: grant.GrantID, Registration: grant.RegistrationID, Generation: grant.Generation, Seq: grant.LeaseSeq, Session: session, Worker: worker, Mode: "getconf"})
	transcript := wlwire.Transcript("WL-VPN-POP-1", exporter, challengeID, nonce, payload)
	digest := sha256.Sum256(transcript)
	proof, _ := ecdsa.SignASN1(rand.Reader, key, digest[:])
	send(wlwire.ID{2}, clientAuthBody{V: 1, Op: "VPN_AUTH", Challenge: wlwire.Encode(challengeID), Payload: wlwire.Encode(payload), Proof: wlwire.Encode(proof)})
	reply := recv()
	result := <-serverResult
	code, _ := reply["code"].(string)
	return client, result.conn, result.err, code
}

func TestManagedDuplicateSessionWorkerReturnsAuthRequired(t *testing.T) {
	identity, grant, key := managedFixture(t)
	cert, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := dtls.Listen("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &dtls.Config{Certificates: []tls.Certificate{cert}, ExtendedMasterSecret: dtls.RequireExtendedMasterSecret})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	const session = "fixture-session-0001"
	firstClient, firstServer, firstErr, firstCode := managedAuthAttempt(t, ctx, listener, identity, grant, key, session, "0")
	if firstErr != nil || firstCode != "" || firstServer == nil {
		t.Fatalf("first auth failed: code=%q err=%v", firstCode, firstErr)
	}
	defer firstClient.Close()
	if got := clientTestWorkerCount(grant.GrantID); got != 1 {
		t.Fatalf("first worker count = %d, want 1", got)
	}

	secondClient, secondServer, secondErr, secondCode := managedAuthAttempt(t, ctx, listener, identity, grant, key, session, "0")
	defer secondClient.Close()
	if secondServer != nil || secondErr == nil || secondErr.Error() != "SESSION_NOT_READY" || secondCode != "SESSION_NOT_READY" {
		t.Fatalf("duplicate auth: code=%q err=%v conn=%v", secondCode, secondErr, secondServer)
	}
	if got := clientTestWorkerCount(grant.GrantID); got != 1 {
		t.Fatalf("duplicate changed worker count to %d", got)
	}
	entry := db.Passwords[identity.password]
	if entry == nil || entry.ClientTest == nil || entry.ClientTest.Revoked || entry.ClientTest.GrantID != grant.GrantID {
		t.Fatal("duplicate changed grant or revoke state")
	}
	if cap, _ := configuredAccessRuntimeLimits(); cap != 2 {
		t.Fatalf("duplicate changed configured cap to %d", cap)
	}

	if err := firstServer.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for clientTestWorkerCount(grant.GrantID) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := clientTestWorkerCount(grant.GrantID); got != 0 {
		t.Fatalf("old worker count after close = %d", got)
	}
	thirdClient, thirdServer, thirdErr, thirdCode := managedAuthAttempt(t, ctx, listener, identity, grant, key, session, "0")
	defer thirdClient.Close()
	if thirdErr != nil || thirdCode != "" || thirdServer == nil {
		t.Fatalf("worker not admitted after old close: code=%q err=%v", thirdCode, thirdErr)
	}
	defer thirdServer.Close()
}
