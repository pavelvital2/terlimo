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
	"testing"
	"testing/synctest"
	"time"

	"wg-turn-client/accountaccess"
	"wg-turn-client/servicechannel"
	"wg-turn-client/wlwire"
)

// Real signature→candidate, channel framing, PoP Session, strict /me and Store;
// only network I/O, durable writer and clock are fake. No device/server/sleep.
func TestRecoveryColdPreparationBudget(t *testing.T) {
	for _, tc := range []struct {
		name                                          string
		connect, challenge, session, me, commit       time.Duration
		pause, cancel, badMe, signDelay, preAuthDelay bool
		wantOK                                        bool
		elapsed                                       time.Duration
	}{
		{name: "cold_over_15s_one_commit", connect: 8 * time.Second, challenge: 8 * time.Second, session: 9 * time.Second, me: 2 * time.Second, wantOK: true, elapsed: 27 * time.Second},
		{name: "all_phases_near_limit", connect: 19 * time.Second, challenge: 12 * time.Second, session: 12 * time.Second, me: 9 * time.Second, wantOK: true, elapsed: 52 * time.Second},
		{name: "total_55s_includes_preparation", preAuthDelay: true, connect: 19 * time.Second, challenge: 12 * time.Second, session: 12 * time.Second, me: 9 * time.Second, elapsed: 55 * time.Second},
		{name: "connect_20s_exceeded", connect: 21 * time.Second, elapsed: 20 * time.Second},
		{name: "auth_aggregate_not_reset", connect: 2 * time.Second, challenge: 14 * time.Second, session: 14 * time.Second, elapsed: 27 * time.Second},
		{name: "signing_spends_auth_phase", challenge: time.Second, signDelay: true, elapsed: 25 * time.Second},
		{name: "me_10s_exceeded", connect: 2 * time.Second, me: 11 * time.Second, elapsed: 12 * time.Second},
		{name: "failed_fresh_me", badMe: true},
		{name: "terminal_cancel", connect: 8 * time.Second, cancel: true, elapsed: 3 * time.Second},
		{name: "captcha_pauses_existing_clocks", connect: 8 * time.Second, challenge: 8 * time.Second, session: 9 * time.Second, me: 2 * time.Second, pause: true, wantOK: true, elapsed: 87 * time.Second},
		{name: "commit_uses_status_remainder", me: 8 * time.Second, commit: 3 * time.Second, elapsed: 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var vector struct {
					Key  string              `json:"public_key_b64"`
					Code string              `json:"code"`
					Seed servicechannel.Seed `json:"expected_seed"`
				}
				raw, err := os.ReadFile("servicechannel/testdata/recovery-v1/fixture.json")
				if err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(raw, &vector); err != nil {
					t.Fatal(err)
				}
				old := vector.Seed
				old.Revision = "1"
				oldRaw, _ := json.Marshal(old)
				writes := 0
				persist := func(ctx context.Context, _ []byte) error {
					if tc.commit > 0 {
						select {
						case <-time.After(tc.commit):
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					writes++
					return nil
				}
				transport, store, err := newMobileTransportAndStore(managedStart{MobileBaseURL: "https://recovery.invalid", MobileEnvironment: "test", ServiceSeed: string(oldRaw), RecoveryCode: vector.Code, RecoveryVerifyKeyB64: vector.Key}, persist)
				if err != nil {
					t.Fatal(err)
				}
				recovery := transport.(*mobileRecoveryTransport)
				defer recovery.Close()
				before, _ := store.State()
				var output bytes.Buffer
				configureCatalogStages(recovery, newManagedBridge(&output, "budget", func() {}))
				if recovery.Channel.Catalog == nil {
					t.Fatal("wrapped Doer omitted cold policy")
				}
				originalPolicy := recovery.Channel.Catalog
				auth := newWiringFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.cancel {
					time.AfterFunc(3*time.Second, cancel)
				}
				calls := map[string]int{}
				var dialCtx context.Context
				recovery.Channel.Establishment = servicechannel.EstablishFunc(func(lifetime context.Context, _ servicechannel.Seed) (net.Conn, func(), error) {
					dialCtx = lifetime
					if tc.pause {
						budget := budgetFromContext(lifetime)
						if budget == nil {
							t.Fatal("CAPTCHA budget handle lost")
						}
						if paused, ok := budget.PauseForWait(); !paused || !ok {
							t.Fatal("could not pause")
						}
						time.Sleep(time.Minute)
						if lifetime.Err() != nil {
							t.Fatal("CAPTCHA expired preparation")
						}
						budget.ResumeAfterWait()
					}
					select {
					case <-time.After(tc.connect):
					case <-lifetime.Done():
						return nil, nil, lifetime.Err()
					}
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
							id, data, err := assembler.Add(buf[:n], time.Now())
							if err != nil {
								return
							}
							if data == nil {
								continue
							}
							var f struct {
								Method, Path string
								Headers      map[string]string
								Body         string `json:"body_b64"`
							}
							if json.Unmarshal(data, &f) != nil {
								return
							}
							calls[f.Path]++
							delay := time.Duration(0)
							switch f.Path {
							case "/api/mobile/v1/auth/challenge":
								delay = tc.challenge
							case "/api/mobile/v1/auth/session":
								delay = tc.session
							case "/api/mobile/v1/me":
								delay = tc.me
							default:
								t.Error("unexpected route", f.Path)
								return
							}
							select {
							case <-time.After(delay):
							case <-lifetime.Done():
								return
							}
							body, _ := base64.RawURLEncoding.DecodeString(f.Body)
							req := httptest.NewRequest(f.Method, "https://recovery.invalid"+f.Path, bytes.NewReader(body))
							for k, v := range f.Headers {
								req.Header.Set(k, v)
							}
							recorder := httptest.NewRecorder()
							if tc.badMe && f.Path == "/api/mobile/v1/me" {
								recorder.WriteHeader(http.StatusOK)
								recorder.Write([]byte(`{}`))
							} else {
								auth.handler().ServeHTTP(recorder, req)
							}
							wire, _ := json.Marshal(map[string]any{"v": 1, "request_id": servicechannel.RequestID(id), "status": recorder.Code, "headers": map[string]string{"Content-Type": "application/json"}, "body_b64": base64.RawURLEncoding.EncodeToString(recorder.Body.Bytes())})
							frames, _ := wlwire.ServiceFrames(id, true, wire)
							for _, f := range frames {
								if _, err = peer.Write(f); err != nil {
									return
								}
							}
							assembler = wlwire.ServiceAssembler{}
						}
					}()
					return client, func() { client.Close(); peer.Close() }, nil
				})
				session, err := accountaccess.NewMobileSession(accountaccess.MobileConfig{BaseURL: "https://recovery.invalid", Environment: accountaccess.EnvironmentTest, SPKIDER: auth.spkiDER, HTTP: recovery, DisableEnrollment: true, Now: func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) }, Signer: func(ctx context.Context, payload []byte) ([]byte, error) {
					if tc.signDelay {
						select {
						case <-time.After(26 * time.Second):
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					sum := sha256.Sum256(payload)
					return ecdsa.SignASN1(rand.Reader, auth.key, sum[:])
				}})
				if err != nil {
					t.Fatal(err)
				}
				coordinator, err := accountaccess.NewCoordinator(accountaccess.Options{Client: &accountaccess.Client{BaseURL: "https://recovery.invalid/api/mobile/v1", HTTP: recovery, Tokens: session}, AttemptID: "budget", Store: &accountaccess.MemoryReceiptStore{}, Subject: session.Subject})
				if err != nil {
					t.Fatal(err)
				}
				runner, err := accountaccess.NewRunner(accountaccess.RunnerConfig{Session: session, Coordinator: coordinator, Emit: func(context.Context, map[string]any) error { t.Error("catalog loop ran"); return nil }})
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				err = applyRecovery(ctx, recovery, func(operation context.Context) error {
					if tc.preAuthDelay {
						time.Sleep(4 * time.Second)
					}
					return runner.AuthenticateServiceOnce(operation)
				})
				elapsed := time.Since(start)
				recovery.Close()
				synctest.Wait()
				after, _ := store.State()
				if (err == nil) != tc.wantOK {
					t.Fatalf("err=%v routes=%v elapsed=%v", err, calls, elapsed)
				}
				if tc.wantOK {
					if writes != 1 || bytes.Equal(before, after) || calls["/api/mobile/v1/auth/session"] != 1 || calls["/api/mobile/v1/me"] != 1 {
						t.Fatalf("commit/auth count mismatch writes=%d calls=%v", writes, calls)
					}
				} else if writes != 0 || !bytes.Equal(before, after) {
					t.Fatal("failure changed last-good")
				}
				if tc.elapsed > 0 && elapsed != tc.elapsed {
					t.Fatalf("elapsed=%v want=%v", elapsed, tc.elapsed)
				}
				if dialCtx != nil && dialCtx.Err() == nil {
					t.Fatal("transport lifetime leaked")
				}
				if recovery.Channel.Catalog != originalPolicy || originalPolicy.TimeoutContext != nil {
					t.Fatal("operation policy leaked")
				}
				if output.Len() != 0 {
					t.Fatal("recovery emitted catalog lifecycle")
				}
				if tc.cancel && !errors.Is(err, context.Canceled) {
					t.Fatal("terminal cancel lost", err)
				}
			})
		})
	}
}
