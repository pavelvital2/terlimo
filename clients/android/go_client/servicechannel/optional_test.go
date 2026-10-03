package servicechannel

// Actual in-memory service fixtures exercise optional scheduling/connection gates.
// These tests do not repeat recovery parsing, auth, persistence or manual recovery.

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"testing"
	"time"

	"wg-turn-client/wlwire"
)

func optionalSeedFixture(t *testing.T, reply func(requestFrame, wlwire.ID) fixtureResponse) (*Doer, *fixtureEstablisher) {
	t.Helper()
	if reply == nil {
		reply = func(_ requestFrame, id wlwire.ID) fixtureResponse {
			return fixtureResponse{body: responseJSON(t, id, http.StatusOK, nil, []byte(`{"ok":true}`))}
		}
	}
	fixture := newFixtureEstablisher(t, reply)
	doer := fixtureDoer(t, fixture)
	t.Cleanup(func() {
		_ = doer.Close()
		fixture.mu.Lock()
		conns := append([]*loopConn(nil), fixture.conns...)
		fixture.mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return doer, fixture
}

func optionalSeedGET(doer *Doer, ctx context.Context, path string) error {
	request, err := newTestRequest(http.MethodGet, testOrigin+"/api/mobile/v1"+path, nil)
	if err != nil {
		return err
	}
	response, err := doer.Do(request.WithContext(ctx))
	if response != nil {
		_ = response.Body.Close()
	}
	return err
}

func TestOptionalSeedNeverEstablishesColdStaleOrFull(t *testing.T) {
	for _, mode := range []string{"cold", "idle", "full", "changed-endpoint"} {
		t.Run(mode, func(t *testing.T) {
			doer, fixture := optionalSeedFixture(t, nil)
			if mode != "cold" {
				if err := optionalSeedGET(doer, context.Background(), "/me"); err != nil {
					t.Fatal(err)
				}
			}
			channel := doer.Channel
			channel.mu.Lock()
			oldConn, oldIdentity := channel.conn, cloneSeed(channel.seed)
			switch mode {
			case "idle":
				channel.lastReply = time.Now().Add(-maxSessionIdle - time.Millisecond)
			case "full":
				channel.requests = maxSessionRequests
			}
			channel.mu.Unlock()
			if mode == "changed-endpoint" {
				updated := cloneSeed(oldIdentity)
				updated.Revision, updated.PeerIP = "2", "198.51.100.23"
				if err := doer.Seeds.Update(SourceCached, seedJSON(t, updated)); err != nil {
					t.Fatal(err)
				}
			}
			beforeDials := fixture.dialCount()
			beforeFrames, _, _, _ := fixture.snapshot()
			ctx, finish, ok := doer.BeginOptional(context.Background())
			if !ok {
				t.Fatal("idle optional lease unexpectedly refused")
			}
			defer finish()
			if err := optionalSeedGET(doer, ctx, "/service-seed"); !errors.Is(err, ErrTransportFailed) {
				t.Fatalf("unusable optional connection: %v", err)
			}
			afterFrames, _, _, _ := fixture.snapshot()
			if fixture.dialCount() != beforeDials || len(afterFrames) != len(beforeFrames) {
				t.Fatal("optional established or sent on an unusable connection")
			}
			channel.mu.Lock()
			retainedConn, retainedIdentity := channel.conn, cloneSeed(channel.seed)
			channel.mu.Unlock()
			if retainedConn != oldConn || recoverySeedDigest(retainedIdentity) != recoverySeedDigest(oldIdentity) {
				t.Fatal("optional refusal evicted or relabeled the existing connection")
			}
		})
	}
}

func TestOptionalSeedWarmReuseLeaseOutlivesGET(t *testing.T) {
	doer, fixture := optionalSeedFixture(t, nil)
	if err := optionalSeedGET(doer, context.Background(), "/me"); err != nil {
		t.Fatal(err)
	}
	doer.Channel.mu.Lock()
	oldConn := doer.Channel.conn
	doer.Channel.mu.Unlock()
	ctx, finish, ok := doer.BeginOptional(context.Background())
	if !ok {
		t.Fatal("warm optional lease refused")
	}
	defer finish()
	if err := optionalSeedGET(doer, ctx, "/service-seed"); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil || fixture.dialCount() != 1 {
		t.Fatal("successful optional GET canceled its caller lease or redialed")
	}
	_, secondFinish, second := doer.BeginOptional(context.Background())
	secondFinish()
	if second {
		t.Fatal("optional lease ended at HTTP return before caller verification/save")
	}
	// Foreground arrival must also cancel an optional caller that is past HTTP but
	// still verifying/staging its result. Its lease remains held until finish.
	if err := optionalSeedGET(doer, context.Background(), "/gateways"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("foreground did not cancel the optional caller's remaining work")
	}
	doer.Channel.mu.Lock()
	currentConn := doer.Channel.conn
	doer.Channel.mu.Unlock()
	if fixture.dialCount() != 1 || currentConn != oldConn {
		t.Fatal("preempting post-HTTP optional work replaced the healthy connection")
	}
	_, heldFinish, held := doer.BeginOptional(context.Background())
	heldFinish()
	if held {
		t.Fatal("canceled optional caller lost its lease before finish")
	}
	finish()
	_, nextFinish, next := doer.BeginOptional(context.Background())
	defer nextFinish()
	if !next {
		t.Fatal("finished caller did not release optional lease")
	}
}

func TestOptionalSeedForegroundPreemptsBlockedExchange(t *testing.T) {
	optionalEntered := make(chan struct{}, 1)
	doer, fixture := optionalSeedFixture(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		if frame.Path == "/api/mobile/v1/service-seed" {
			optionalEntered <- struct{}{}
			return fixtureResponse{block: true}
		}
		return fixtureResponse{body: responseJSON(t, id, http.StatusOK, nil, []byte(`{"ok":true}`))}
	})
	if err := optionalSeedGET(doer, context.Background(), "/me"); err != nil {
		t.Fatal(err)
	}
	ctx, finish, ok := doer.BeginOptional(context.Background())
	if !ok {
		t.Fatal("warm optional lease refused")
	}
	defer finish()
	optionalDone := make(chan error, 1)
	go func() { optionalDone <- optionalSeedGET(doer, ctx, "/service-seed") }()
	select {
	case <-optionalEntered: // real exchange now owns the shared Channel mutex
	case <-time.After(time.Second):
		t.Fatal("optional exchange never reached blocked service fixture")
	}
	foregroundCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	foregroundDone := make(chan error, 1)
	go func() { foregroundDone <- optionalSeedGET(doer, foregroundCtx, "/gateways") }()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("foreground waited behind Channel mutex without canceling optional")
	}
	select {
	case err := <-optionalDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("optional did not exit with cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("preempted optional did not release Channel mutex")
	}
	select {
	case err := <-foregroundDone:
		if err != nil {
			t.Fatalf("foreground failed after bounded cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("foreground did not progress after optional cancellation")
	}
	if fixture.dialCount() != 2 {
		t.Fatal("foreground must replace only the connection terminated by cancellation")
	}
	before := fixture.dialCount()
	if err := optionalSeedGET(doer, ctx, "/service-seed"); !errors.Is(err, context.Canceled) || fixture.dialCount() != before {
		t.Fatalf("canceled optional caller redialed: %v", err)
	}
}

func TestOptionalSeedLeaseRefusedWhileForegroundActive(t *testing.T) {
	foregroundEntered, release := make(chan struct{}), make(chan struct{})
	doer, _ := optionalSeedFixture(t, func(_ requestFrame, id wlwire.ID) fixtureResponse {
		close(foregroundEntered)
		<-release
		return fixtureResponse{body: responseJSON(t, id, http.StatusOK, nil, []byte(`{"ok":true}`))}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	foregroundCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- optionalSeedGET(doer, foregroundCtx, "/me") }()
	select {
	case <-foregroundEntered:
	case <-time.After(time.Second):
		t.Fatal("foreground exchange did not start")
	}
	_, finish, ok := doer.BeginOptional(context.Background())
	finish()
	if ok {
		t.Fatal("optional lease admitted during an active foreground request")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("foreground fixture did not complete")
	}
	_, finish, ok = doer.BeginOptional(context.Background())
	defer finish()
	if !ok {
		t.Fatal("foreground completion did not release optional admission")
	}
}

func TestOptionalSeedPreemptedColdLeaseNeverDials(t *testing.T) {
	doer, fixture := optionalSeedFixture(t, nil)
	ctx, finish, ok := doer.BeginOptional(context.Background())
	if !ok {
		t.Fatal("optional lease refused")
	}
	defer finish()
	doer.PreemptOptional()
	if err := optionalSeedGET(doer, ctx, "/service-seed"); !errors.Is(err, context.Canceled) || fixture.dialCount() != 0 {
		t.Fatalf("preempted cold optional established: %v", err)
	}
}

func TestOptionalSeedAcceptedEndpointDeferredUntilNaturalClose(t *testing.T) {
	doer, fixture := optionalSeedFixture(t, nil)
	doer.Channel.PreserveConnectionSeed = true
	if err := optionalSeedGET(doer, context.Background(), "/me"); err != nil {
		t.Fatal(err)
	}
	channel := doer.Channel
	channel.mu.Lock()
	oldConn, oldIdentity := channel.conn, cloneSeed(channel.seed)
	channel.mu.Unlock()
	updated := cloneSeed(oldIdentity)
	updated.Revision, updated.PeerIP, updated.VKHashes = "2", "198.51.100.24", []string{"accepted-new-hash"}
	newPin := sha256.Sum256([]byte("OPTIONAL SERVICE TEST PIN"))
	updated.DTLSSPKISHA256 = encodeRawURL(newPin[:])
	if err := doer.Seeds.Update(SourceCached, seedJSON(t, updated)); err != nil {
		t.Fatal(err)
	}
	ctx, finish, ok := doer.BeginOptional(context.Background())
	if !ok {
		t.Fatal("warm optional lease refused")
	}
	defer finish()
	if err := optionalSeedGET(doer, ctx, "/service-seed"); err != nil {
		t.Fatal(err)
	}
	finish()
	if err := optionalSeedGET(doer, context.Background(), "/gateways"); err != nil {
		t.Fatal(err)
	}
	channel.mu.Lock()
	retainedConn, retainedIdentity := channel.conn, cloneSeed(channel.seed)
	channel.mu.Unlock()
	if fixture.dialCount() != 1 || retainedConn != oldConn || recoverySeedDigest(retainedIdentity) != recoverySeedDigest(oldIdentity) {
		t.Fatal("accepted future endpoint/pin update relabeled or tore down live connection")
	}
	if err := doer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := optionalSeedGET(doer, context.Background(), "/me"); err != nil {
		t.Fatal(err)
	}
	channel.mu.Lock()
	newConn, newIdentity := channel.conn, cloneSeed(channel.seed)
	channel.mu.Unlock()
	fixture.mu.Lock()
	establishedSeed := cloneSeed(fixture.seeds[len(fixture.seeds)-1])
	fixture.mu.Unlock()
	if fixture.dialCount() != 2 || newConn == oldConn || recoverySeedDigest(newIdentity) != recoverySeedDigest(updated) || recoverySeedDigest(establishedSeed) != recoverySeedDigest(updated) {
		t.Fatal("next natural establishment did not consume accepted endpoint/pin/hash update")
	}
}
