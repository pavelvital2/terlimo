package servicechannel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"wg-turn-client/wlwire"
)

func TestExchangeRoundTrip(t *testing.T) {
	var requestID wlwire.ID
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		requestID = id
		return fixtureResponse{body: responseJSON(t, id, 200, map[string]string{"Content-Type": "application/json"}, []byte(`{"ok":true}`))}
	})
	channel := NewChannel(fixture)
	id := mustID(t, "000102030405060708090a0b0c0d0e0f")
	frame := requestFrame{
		V:         1,
		Op:        "service.http",
		RequestID: RequestID(id),
		Method:    "GET",
		Path:      "/api/mobile/v1/me",
		Headers:   map[string]string{},
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := channel.Exchange(context.Background(), validSeed(), id, payload)
	if err != nil {
		t.Fatal(err)
	}
	received, raws, _, invalid := fixture.snapshot()
	if len(received) != 1 || len(invalid) != 1 || invalid[0] != nil {
		t.Fatalf("fixture request invalid: %v", invalid)
	}
	if !bytes.Equal(raws[0], payload) {
		t.Fatal("fixture did not receive the exact request payload")
	}
	if requestID == (wlwire.ID{}) {
		t.Fatal("fixture never saw a frame id")
	}
	var replyFrame responseFrame
	if err := wlwire.StrictJSON(reply, &replyFrame); err != nil || replyFrame.Status != 200 {
		t.Fatalf("reply not reassembled: %v %+v", err, replyFrame)
	}
}

func TestSequentialMobileRequestsReuseBoundedConnection(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(_ requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200,
			map[string]string{"Content-Type": "application/json"}, []byte(`{"ok":true}`)), keepOpen: true}
	})
	channel := NewChannel(fixture)
	defer channel.Close()
	for i := 0; i < maxSessionRequests+1; i++ {
		var id wlwire.ID
		id[15] = byte(i + 1)
		frame := requestFrame{V: 1, Op: "service.http", RequestID: RequestID(id),
			Method: "GET", Path: "/api/mobile/v1/me", Headers: map[string]string{}}
		payload, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := channel.Exchange(context.Background(), validSeed(), id, payload); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		wantDials := 1
		if i == maxSessionRequests {
			wantDials = 2
		}
		if got := fixture.dialCount(); got != wantDials {
			t.Fatalf("request %d: establishments %d, want %d", i+1, got, wantDials)
		}
	}
}

func TestRequestContextEndsWithoutClosingBoundedTransport(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(_ requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, nil, []byte(`{"ok":true}`)), keepOpen: true}
	})
	channel := NewChannel(EstablishFunc(func(ctx context.Context, seed Seed) (net.Conn, func(), error) {
		conn, cleanup, err := fixture.Establish(ctx, seed)
		if err == nil {
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			original := cleanup
			cleanup = func() { stop(); original() }
		}
		return conn, cleanup, err
	}))
	defer channel.Close()
	for i := byte(1); i <= 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		var id wlwire.ID
		id[15] = i
		frame := requestFrame{V: 1, Op: "service.http", SessionMode: "bounded",
			RequestID: RequestID(id), Method: "GET", Path: "/api/mobile/v1/me", Headers: map[string]string{}}
		payload, _ := json.Marshal(frame)
		_, err := channel.Exchange(ctx, validSeed(), id, payload)
		cancel() // accountaccess ends each HTTP request context after the response
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if fixture.dialCount() != 1 {
		t.Fatalf("request cancellation closed the bounded session: %d dials", fixture.dialCount())
	}
}

func TestExchangeRejectsOversizedPayloadAndFrames(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{}
	})
	channel := NewChannel(fixture)
	id := mustID(t, "000102030405060708090a0b0c0d0e0f")
	if _, err := channel.Exchange(context.Background(), validSeed(), id, bytes.Repeat([]byte{0x61}, wlwire.ServiceMaxFrame+1)); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("oversized payload accepted: %v", err)
	}
	if fixture.dialCount() != 0 {
		t.Fatal("oversized payload reached the establishment")
	}

	// A peer that claims a frame total above the service ceiling is rejected by the
	// shared assembler from its header alone.
	total := uint32(wlwire.ServiceMaxFrame + 1)
	header := make([]byte, 32+wlwire.ServiceFragment)
	copy(header, "WLBS")
	header[4] = wlwire.ServiceVersion
	header[7] = 32
	header[24] = byte(total >> 24)
	header[25] = byte(total >> 16)
	header[26] = byte(total >> 8)
	header[27] = byte(total)
	fixture = newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{rawFrames: [][]byte{header}}
	})
	channel = NewChannel(fixture)
	if _, err := channel.Exchange(context.Background(), validSeed(), id, []byte(`{"v":1}`)); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("oversized peer frame accepted: %v", err)
	}

	// Malformed frames (bad magic, truncated) are rejected the same way.
	for _, garbage := range [][]byte{[]byte("garbage"), bytes.Repeat([]byte{0x00}, 40)} {
		fixture = newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
			return fixtureResponse{rawFrames: [][]byte{garbage}}
		})
		channel = NewChannel(fixture)
		if _, err := channel.Exchange(context.Background(), validSeed(), id, []byte(`{"v":1}`)); !errors.Is(err, ErrBadResponse) {
			t.Fatalf("malformed peer frame accepted: %v", err)
		}
	}
}

func TestExchangeCancellationClosesTerminally(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{block: true}
	})
	channel := NewChannel(fixture)
	var stages []string
	channel.Observe = func(stage string) { stages = append(stages, stage) }
	id := mustID(t, "000102030405060708090a0b0c0d0e0f")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := channel.Exchange(ctx, validSeed(), id, []byte(`{"v":1}`))
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for fixture.dialCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("exchange never dialed")
		}
		time.Sleep(time.Millisecond)
	}
	fixture.mu.Lock()
	client := fixture.conns[0]
	fixture.mu.Unlock()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation not propagated: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exchange did not stop after cancellation")
	}
	select {
	case <-client.closed:
	default:
		t.Fatal("cancellation did not terminate the channel")
	}
	if resets := client.deadlineResets(); resets != 0 {
		t.Fatalf("deadline was reset %d times after cancellation", resets)
	}
	if got := strings.Join(stages, ","); !strings.Contains(got, "FRAME_READ_FAILED,EXCHANGE_CANCELLED") ||
		!strings.Contains(got, "CONNECTION_CLOSED") {
		t.Fatalf("cancellation/cleanup stages missing: %s", got)
	}
}

func TestExchangeMissingEstablishmentFailsClosed(t *testing.T) {
	channel := &Channel{}
	store := NewStore(Config{Environment: "test"})
	if err := store.Update(SourceBuiltin, seedJSON(t, validSeed())); err != nil {
		t.Fatal(err)
	}
	doer, err := NewDoer("https://mobile.example.test", store, channel)
	if err != nil {
		t.Fatal(err)
	}
	request, err := newTestRequest("GET", "https://mobile.example.test/api/mobile/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doer.Do(request); !errors.Is(err, ErrTransportFailed) {
		t.Fatalf("missing establishment did not fail closed: %v", err)
	}
	if _, _, ok := store.Current(); !ok {
		t.Fatal("seed store lost the trusted seed")
	}
}

// TestExchangeFixedTimeoutNotIncreased pins the accepted product bound: the service
// exchange must never extend the 15 s mobile HTTP timeout.
func TestExchangeFixedTimeoutNotIncreased(t *testing.T) {
	if exchangeLimit != 15*time.Second {
		t.Fatalf("service exchange timeout changed: %v", exchangeLimit)
	}
}

func TestEstablishPendingDeadlineAndCancelRemainObservable(t *testing.T) {
	for _, tc := range []struct {
		name, marker string
		timeout      bool
	}{
		{"deadline", "ESTABLISH_TIMEOUT_UNPROCESSED", true},
		{"cancel", "ESTABLISH_CANCEL_UNPROCESSED", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			stages := make(chan string, 8)
			ch := NewChannel(EstablishFunc(func(ctx context.Context, _ Seed) (net.Conn, func(), error) {
				close(entered)
				<-release // Deliberately ignores cancellation, as a stuck seam might.
				return nil, nil, ctx.Err()
			}))
			ch.Observe = func(stage string) { stages <- stage }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.timeout {
				ch.Timeout = 20 * time.Millisecond
			}
			result := make(chan error, 1)
			go func() {
				_, err := ch.Exchange(ctx, validSeed(), mustID(t, "000102030405060708090a0b0c0d0e0f"), []byte(`{"v":1}`))
				result <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("establishment not entered")
			}
			if !tc.timeout {
				cancel()
			}
			deadline := time.After(time.Second)
			for {
				select {
				case stage := <-stages:
					if stage == tc.marker {
						select {
						case <-result:
							t.Fatal("Exchange returned before stuck establishment was released")
						default:
						}
						close(release)
						select {
						case err := <-result:
							if err == nil {
								t.Fatal("expected terminal error")
							}
							terminal := "EXCHANGE_CANCELLED"
							if tc.timeout {
								terminal = "EXCHANGE_TIMEOUT"
							}
							for _, want := range []string{"ESTABLISH_FAILED", terminal} {
								select {
								case got := <-stages:
									if got != want {
										t.Fatalf("stage order: got %s want %s", got, want)
									}
								case <-time.After(time.Second):
									t.Fatalf("missing terminal stage %s", want)
								}
							}
						case <-time.After(time.Second):
							t.Fatal("Exchange did not return after release")
						}
						return
					}
				case <-deadline:
					close(release)
					t.Fatalf("missing %s", tc.marker)
				}
			}
		})
	}
}

func TestEstablishUnprocessedMarkerCannotFollowTerminalWhenObserverBlocks(t *testing.T) {
	for _, tc := range []struct {
		name, marker, terminal string
		timeout                bool
	}{
		{"deadline", "ESTABLISH_TIMEOUT_UNPROCESSED", "EXCHANGE_TIMEOUT", true},
		{"cancel", "ESTABLISH_CANCEL_UNPROCESSED", "EXCHANGE_CANCELLED", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			observerEntered := make(chan struct{})
			unblockObserver := make(chan struct{})
			stages := make(chan string, 8)
			ch := NewChannel(EstablishFunc(func(ctx context.Context, _ Seed) (net.Conn, func(), error) {
				close(entered)
				<-release
				return nil, nil, ctx.Err()
			}))
			ch.Observe = func(stage string) {
				if stage == tc.marker {
					close(observerEntered) // Callback has decided to report before Establish returns.
					<-unblockObserver
				}
				stages <- stage
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.timeout {
				ch.Timeout = 20 * time.Millisecond
			}
			result := make(chan error, 1)
			id := mustID(t, "000102030405060708090a0b0c0d0e0f")
			go func() {
				_, err := ch.Exchange(ctx, validSeed(), id, []byte(`{"v":1}`))
				result <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("establishment not entered")
			}
			if !tc.timeout {
				cancel()
			}
			select {
			case <-observerEntered:
			case <-time.After(time.Second):
				t.Fatal("pending observer did not start")
			}
			close(release)
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("expected cancellation or timeout")
				}
			case <-time.After(time.Second):
				t.Fatal("blocked observer deadlocked Exchange")
			}
			if got := <-stages; got != "ESTABLISH_BEGIN" {
				t.Fatalf("unexpected first stage %q", got)
			}
			select {
			case stage := <-stages:
				t.Fatalf("terminal emitted while pending observer blocked: %s", stage)
			default:
			}
			close(unblockObserver)
			for _, want := range []string{tc.marker, "ESTABLISH_FAILED", tc.terminal} {
				select {
				case got := <-stages:
					if got != want {
						t.Fatalf("stage order: got %s want %s", got, want)
					}
				case <-time.After(time.Second):
					t.Fatalf("missing %s", want)
				}
			}
		})
	}
}

func TestExchangeTransportErrorIsFixed(t *testing.T) {
	if got := transportError(context.Background(), errors.New("peer said secret /tmp/x")).Error(); got != ErrTransportFailed.Error() {
		t.Fatalf("unexpected transport mapping: %s", got)
	}
	if got := transportError(context.Background(), errors.New("TRUST_FAILED")).Error(); !strings.Contains(got, "TRUST_FAILED") {
		t.Fatalf("fixed safe code was masked: %s", got)
	}
}

func TestDiagnosticStagesLocateEstablishmentAndFrameBoundary(t *testing.T) {
	var stages []string
	failing := NewChannel(EstablishFunc(func(context.Context, Seed) (net.Conn, func(), error) {
		return nil, nil, errors.New("private peer detail")
	}))
	failing.Observe = func(stage string) { stages = append(stages, stage) }
	id := mustID(t, "000102030405060708090a0b0c0d0e0f")
	if _, err := failing.Exchange(context.Background(), validSeed(), id, []byte(`{"v":1}`)); !errors.Is(err, ErrTransportFailed) {
		t.Fatalf("unexpected establishment result: %v", err)
	}
	if got := strings.Join(stages, ","); got != "ESTABLISH_BEGIN,ESTABLISH_FAILED" {
		t.Fatalf("unexpected establishment stages: %s", got)
	}

	stages = nil
	fixture := newFixtureEstablisher(t, func(_ requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, nil, []byte(`{"ok":true}`))}
	})
	channel := NewChannel(fixture)
	channel.Observe = func(stage string) { stages = append(stages, stage) }
	frame := requestFrame{V: 1, Op: "service.http", RequestID: RequestID(id),
		Method: "GET", Path: "/api/mobile/v1/me", Headers: map[string]string{}}
	payload, _ := json.Marshal(frame)
	if _, err := channel.Exchange(context.Background(), validSeed(), id, payload); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stages, ","); got != "ESTABLISH_BEGIN,ESTABLISH_OK,FRAME_WRITE_BEGIN,FRAME_WRITE_OK,FRAME_READ_BEGIN,FRAME_READ_OK" {
		t.Fatalf("unexpected round-trip stages: %s", got)
	}
}
