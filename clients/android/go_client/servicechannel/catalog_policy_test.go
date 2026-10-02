package servicechannel

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"wg-turn-client/wlwire"
)

func catalogTestPayload(t *testing.T, sequence byte) (wlwire.ID, []byte) {
	t.Helper()
	var id wlwire.ID
	id[15] = sequence
	payload, err := json.Marshal(requestFrame{V: 1, Op: "service.http", SessionMode: "bounded",
		RequestID: RequestID(id), Method: "GET", Path: "/api/mobile/v1/me", Headers: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	return id, payload
}

func TestCatalogSlowEstablishDoesNotSpendRequestBudgetAndReusesSession(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(_ requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, nil, []byte(`{}`)), keepOpen: true}
	})
	var phases []string
	c := NewChannel(EstablishFunc(func(ctx context.Context, seed Seed) (net.Conn, func(), error) {
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		conn, cleanup, err := fixture.Establish(ctx, seed)
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		return conn, func() { stop(); cleanup() }, err
	}))
	defer c.Close()
	c.Timeout = 50 * time.Millisecond // the old combined budget would fail
	c.Catalog = &CatalogPolicy{ConnectTimeout: time.Second, DeviceTimeout: 100 * time.Millisecond,
		Emit: func(ctx context.Context, stage string) error {
			phases = append(phases, stage)
			if stage == "checking_device" {
				deadline, _ := ctx.Deadline()
				if time.Until(deadline) < 50*time.Millisecond {
					t.Error("establishment consumed the request budget")
				}
			}
			return nil
		}}
	for seq := byte(1); seq <= 2; seq++ {
		id, payload := catalogTestPayload(t, seq)
		ctx, cancel := context.WithCancel(WithRequestClass(context.Background(), "AUTH"))
		_, err := c.Exchange(ctx, validSeed(), id, payload)
		cancel()
		if err != nil {
			t.Fatalf("request %d: %v", seq, err)
		}
	}
	if fixture.dialCount() != 1 || !reflect.DeepEqual(phases, []string{"connecting_server", "checking_device", "checking_device"}) {
		t.Fatalf("dials=%d phases=%v", fixture.dialCount(), phases)
	}
}

func TestCatalogCallerDeadlineStillWinsDuringEstablish(t *testing.T) {
	var phases []string
	c := NewChannel(EstablishFunc(func(ctx context.Context, _ Seed) (net.Conn, func(), error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}))
	c.Catalog = &CatalogPolicy{ConnectTimeout: time.Second, DeviceTimeout: time.Second,
		Emit: func(_ context.Context, stage string) error { phases = append(phases, stage); return nil }}
	ctx, cancel := context.WithTimeout(WithRequestClass(context.Background(), "AUTH"), 25*time.Millisecond)
	defer cancel()
	id, payload := catalogTestPayload(t, 1)
	_, err := c.Exchange(ctx, validSeed(), id, payload)
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(phases, []string{"connecting_server"}) {
		t.Fatalf("err=%v phases=%v", err, phases)
	}
}

func TestCatalogAuthBudgetIsNotCappedByLegacyFifteenSeconds(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(_ requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, nil, []byte(`{}`))}
	})
	c := NewChannel(fixture)
	defer c.Close()
	seen := false
	c.Catalog = &CatalogPolicy{ConnectTimeout: 20 * time.Second, DeviceTimeout: 25 * time.Second,
		Emit: func(ctx context.Context, stage string) error {
			if stage == "checking_device" {
				seen = true
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) < 24*time.Second {
					t.Error("AUTH budget still inherits the old combined limit")
				}
			}
			return nil
		}}
	id, payload := catalogTestPayload(t, 1)
	_, err := c.Exchange(WithRequestClass(context.Background(), "AUTH"), validSeed(), id, payload)
	if err != nil || !seen {
		t.Fatalf("err=%v stage=%v", err, seen)
	}
}

func TestCatalogRequestBudgetAndCancellationCloseConnection(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[cancelRequest], func(t *testing.T) {
			fixture := newFixtureEstablisher(t, func(_ requestFrame, _ wlwire.ID) fixtureResponse {
				return fixtureResponse{block: true}
			})
			c := NewChannel(fixture)
			defer c.Close()
			ctx, cancel := context.WithCancel(WithRequestClass(context.Background(), "ME"))
			defer cancel()
			c.Catalog = &CatalogPolicy{ConnectTimeout: time.Second, StatusTimeout: 25 * time.Millisecond,
				Emit: func(_ context.Context, stage string) error {
					if cancelRequest && stage == "subscription_status" {
						cancel()
					}
					return nil
				}}
			id, payload := catalogTestPayload(t, 1)
			_, err := c.Exchange(ctx, validSeed(), id, payload)
			want := context.DeadlineExceeded
			if cancelRequest {
				want = context.Canceled
			}
			if !errors.Is(err, want) || c.conn != nil {
				t.Fatalf("err=%v conn retained=%v", err, c.conn != nil)
			}
		})
	}
}

func TestCatalogPolicyDoesNotChangePaymentDeadline(t *testing.T) {
	var emitted bool
	c := NewChannel(EstablishFunc(func(ctx context.Context, _ Seed) (net.Conn, func(), error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}))
	c.Timeout = 25 * time.Millisecond
	c.Catalog = &CatalogPolicy{ConnectTimeout: time.Second, DeviceTimeout: time.Second,
		Emit: func(context.Context, string) error { emitted = true; return nil }}
	id, payload := catalogTestPayload(t, 1)
	_, err := c.Exchange(WithRequestClass(context.Background(), "PAYMENTS"), validSeed(), id, payload)
	if !errors.Is(err, context.DeadlineExceeded) || emitted {
		t.Fatalf("err=%v emitted=%v", err, emitted)
	}
}

func TestCatalogStageDeliveryFailurePreventsRequest(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(_ requestFrame, _ wlwire.ID) fixtureResponse { return fixtureResponse{} })
	c := NewChannel(fixture)
	defer c.Close()
	broken := errors.New("bridge closed")
	c.Catalog = &CatalogPolicy{ConnectTimeout: time.Second, CatalogTimeout: time.Second,
		Emit: func(_ context.Context, stage string) error {
			if stage == "loading_catalog" {
				return broken
			}
			return nil
		}}
	id, payload := catalogTestPayload(t, 1)
	_, err := c.Exchange(WithRequestClass(context.Background(), "GATEWAYS"), validSeed(), id, payload)
	requests, _, _, _ := fixture.snapshot()
	if !errors.Is(err, broken) || len(requests) != 0 || c.conn != nil {
		t.Fatalf("err=%v requests=%d retained=%v", err, len(requests), c.conn != nil)
	}
}
