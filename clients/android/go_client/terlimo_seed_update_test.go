package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"wg-turn-client/accountaccess"
	"wg-turn-client/servicechannel"
	"wg-turn-client/wlwire"
)

func TestIdleSeedNativeVerifiedReadAndFailureRetention(t *testing.T) {
	raw, err := os.ReadFile("testdata/recovery-v1-server-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Key  string              `json:"public_key_b64"`
		Seed servicechannel.Seed `json:"seed"`
		HTTP map[string]any      `json:"http_example"`
	}
	if json.Unmarshal(raw, &vector) != nil {
		t.Fatal("fixture invalid")
	}
	for _, mode := range []string{"valid", "invalid-code", "unavailable", "cancelled", "changed-session", "writer-refused", "next-cycle-due"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			auth := newWiringFixture(t)
			server := httptest.NewServer(auth.handler())
			defer server.Close()
			session, err := accountaccess.NewMobileSession(accountaccess.MobileConfig{BaseURL: server.URL, Environment: accountaccess.EnvironmentTest,
				SPKIDER: auth.spkiDER, HTTP: server.Client(), Now: func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) },
				Signer: func(_ context.Context, body []byte) ([]byte, error) {
					sum := sha256.Sum256(body)
					return ecdsa.SignASN1(rand.Reader, auth.key, sum[:])
				}})
			if err != nil {
				t.Fatal(err)
			}
			if err = session.Ensure(ctx); err != nil {
				t.Fatal(err)
			}
			auth.mu.Lock()
			authBefore := auth.requests
			auth.mu.Unlock()
			old := vector.Seed
			old.Revision = "1"
			old.PeerIP = "192.0.2.9"
			oldRaw, _ := json.Marshal(old)
			writes := 0
			store := servicechannel.NewStore(servicechannel.Config{Environment: "test", Persist: func(persistCtx context.Context, _ []byte) error {
				if persistCtx.Err() != nil {
					t.Fatal("cancelled optional reached writer")
				}
				if epoch, ok := persistCtx.Value(optionalSeedEpochKey{}).(uint64); !ok || epoch != 7 {
					t.Fatal("writer lost owner epoch")
				}
				if mode == "writer-refused" {
					return errors.New("fixed writer rejection")
				}
				writes++
				return nil
			}})
			if err = store.Update(servicechannel.SourceBuiltin, oldRaw); err != nil {
				t.Fatal(err)
			}
			before, _ := store.State()
			envelope := map[string]any{}
			for k, v := range vector.HTTP {
				envelope[k] = v
			}
			// Preserve the exact server vector; correct only its explicitly reported34hex metadata here.
			envelope["request_id"] = "00112233445566778899aabbccddeeff"
			if mode == "invalid-code" {
				envelope["recovery_code"] = "TR1.truncated"
			}
			body, _ := json.Marshal(envelope)
			requests, dials := 0, 0
			channel := servicechannel.NewChannel(servicechannel.EstablishFunc(func(context.Context, servicechannel.Seed) (net.Conn, func(), error) {
				dials++
				client, peer := net.Pipe()
				go func() {
					defer peer.Close()
					assembler := wlwire.ServiceAssembler{}
					buf := make([]byte, 8192)
					for {
						n, err := peer.Read(buf)
						if err != nil {
							return
						}
						id, request, err := assembler.Add(buf[:n], time.Now())
						if err != nil {
							return
						}
						if request == nil {
							continue
						}
						var frame struct {
							Path    string            `json:"path"`
							Headers map[string]string `json:"headers"`
						}
						if json.Unmarshal(request, &frame) != nil {
							return
						}
						status := 200
						if frame.Path == "/api/mobile/v1/service-seed" {
							requests++
							if frame.Headers["Authorization"] != "Bearer "+auth.bearerToken {
								return
							}
							if mode == "unavailable" {
								status = 503
							}
							if mode == "cancelled" {
								cancel()
								return
							}
							if mode == "changed-session" {
								session.Refresh()
							}
						}
						response, _ := json.Marshal(map[string]any{"v": 1, "request_id": servicechannel.RequestID(id), "status": status,
							"headers": map[string]string{"Content-Type": "application/json"}, "body_b64": base64.RawURLEncoding.EncodeToString(body)})
						frames, _ := wlwire.ServiceFrames(id, true, response)
						for _, part := range frames {
							if _, err = peer.Write(part); err != nil {
								return
							}
						}
						assembler = wlwire.ServiceAssembler{}
					}
				}()
				return client, func() { client.Close(); peer.Close() }, nil
			}))
			doer, err := servicechannel.NewDoer("https://mobile.invalid", store, channel)
			if err != nil {
				t.Fatal(err)
			}
			defer doer.Close()
			warm, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://mobile.invalid/api/mobile/v1/me", nil)
			response, err := doer.Do(warm)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			b := newManagedBridge(&bytes.Buffer{}, "attempt", cancel)
			b.seedUpdateEpoch = 7
			b.preemptSeed = doer.PreemptOptional
			mobile := &managedMobile{bridge: b, seedDoer: doer, seedKey: vector.Key, idleSeedPublished: true, session: session,
				controller: &managedController{start: managedStart{MobileEnvironment: "test"}}, client: &accountaccess.Client{BaseURL: "https://mobile.invalid/api/mobile/v1", HTTP: doer, Tokens: session}}
			nextCycle := time.Now().Add(time.Minute)
			if mode == "next-cycle-due" {
				nextCycle = time.Now().Add(-time.Second)
			}
			if !mobile.updateSeedAtIdle(ctx, nextCycle) {
				t.Fatal("optional attempt not consumed")
			}
			after, _ := store.State()
			if mode == "valid" {
				if writes != 1 || bytes.Equal(before, after) {
					t.Fatal("authorized signed update not durably saved")
				}
				fresh := servicechannel.NewStore(servicechannel.Config{Environment: "test"})
				_ = fresh.Update(servicechannel.SourceBuiltin, oldRaw)
				if err = fresh.LoadState(after); err != nil {
					t.Fatal(err)
				}
				current, _, _ := fresh.Current()
				if current.PeerIP != vector.Seed.PeerIP {
					t.Fatal("next connection lost accepted peer")
				}
			} else if writes != 0 || !bytes.Equal(before, after) {
				t.Fatal("failed optional changed durable seed")
			}
			auth.mu.Lock()
			authAfter := auth.requests
			auth.mu.Unlock()
			wantRequests := 1
			if mode == "next-cycle-due" {
				wantRequests = 0
			}
			if authAfter != authBefore || requests != wantRequests || dials != 1 {
				t.Fatalf("extra auth/request/dial: %d %d %d", authAfter-authBefore, requests, dials)
			}
		})
	}
}

func TestIdleSeedBridgeMetadataAndPublicationFence(t *testing.T) {
	b := newManagedBridge(&bytes.Buffer{}, "attempt", func() {})
	preempts := 0
	b.preemptSeed = func() { preempts++ }
	message := bridgeMessage{"type": "device_wake", "v": float64(1), "attempt_id": "attempt", "lifecycle_revision": float64(1), "seed_update_epoch": "9"}
	if !b.preemptSeedUpdate(message) || len(message) != 4 || b.seedUpdateEpoch != 9 || preempts != 1 || !b.seedReceiptBusy.Load() {
		t.Fatal("metadata altered fixed command shape")
	}
	b.finishSeedReceipt()
	if b.seedReceiptBusy.Load() {
		t.Fatal("completed receipt remained busy")
	}
	_ = b.preemptSeedUpdate(bridgeMessage{"type": "payment_get", "seed_update_epoch": "8"})
	if b.seedUpdateEpoch != 9 {
		t.Fatal("older queued command regressed epoch")
	}
	if b.preemptSeedUpdate(bridgeMessage{"type": "persist_result", "seed_update_epoch": "10"}) {
		t.Fatal("reply gained control authority")
	}
	for _, bad := range []string{"-1", strings.Repeat("9", 22), "x"} {
		if b.preemptSeedUpdate(bridgeMessage{"type": "payment_get", "seed_update_epoch": bad}) {
			t.Fatal("bad epoch accepted")
		}
	}
	// Unpublished catalogue (including failed browse emission) cannot initiate optional I/O.
	m := &managedMobile{}
	if m.updateSeedAtIdle(context.Background(), time.Now().Add(time.Minute)) {
		t.Fatal("unpublished catalogue consumed optional attempt")
	}
}
