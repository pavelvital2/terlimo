package servicechannel

// These isolated fixtures exercise the real Store -> Doer -> Channel path. They
// do not repeat parser/persistence tests or establish any live service connection.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

	"wg-turn-client/wlwire"
)

func recoveryChannelFixture(t *testing.T) (*Doer, *fixtureEstablisher) {
	t.Helper()
	fixture := newFixtureEstablisher(t, func(_ requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, http.StatusOK, nil, []byte(`{"ok":true}`))}
	})
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

func recoveryChannelRead(t *testing.T, doer *Doer) {
	t.Helper()
	request, err := newTestRequest(http.MethodGet, testOrigin+"/api/mobile/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := doer.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}

func TestRecoveryChannelHashRevisionReuseThenClose(t *testing.T) {
	doer, fixture := recoveryChannelFixture(t)
	recoveryChannelRead(t, doer)
	channel := doer.Channel
	channel.mu.Lock()
	oldConn, oldIdentity := channel.conn, cloneSeed(channel.seed)
	channel.mu.Unlock()
	updated := cloneSeed(oldIdentity)
	updated.Revision, updated.VKHashes = "2", []string{"updated-trusted-hash"}
	if err := doer.Seeds.Update(SourceCached, seedJSON(t, updated)); err != nil {
		t.Fatal(err)
	}
	recoveryChannelRead(t, doer)
	channel.mu.Lock()
	reusedConn, reusedIdentity := channel.conn, cloneSeed(channel.seed)
	channel.mu.Unlock()
	if fixture.dialCount() != 1 || reusedConn != oldConn {
		t.Fatal("same endpoint hash/revision update replaced the working connection")
	}
	if recoverySeedDigest(reusedIdentity) != recoverySeedDigest(oldIdentity) {
		t.Fatal("existing connection was relabeled with the new seed")
	}
	if current, _, _ := doer.Seeds.Current(); recoverySeedDigest(current) != recoverySeedDigest(updated) {
		t.Fatal("trusted update was not available for the next establishment")
	}
	if err := doer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-oldConn.(*loopConn).closed:
	default:
		t.Fatal("ordinary Close did not terminate the old connection")
	}
	recoveryChannelRead(t, doer)
	channel.mu.Lock()
	newConn, newIdentity := channel.conn, cloneSeed(channel.seed)
	channel.mu.Unlock()
	fixture.mu.Lock()
	establishedSeed := cloneSeed(fixture.seeds[len(fixture.seeds)-1])
	fixture.mu.Unlock()
	if fixture.dialCount() != 2 || newConn == oldConn || recoverySeedDigest(newIdentity) != recoverySeedDigest(updated) || recoverySeedDigest(establishedSeed) != recoverySeedDigest(updated) {
		t.Fatal("next natural establishment did not consume updated hashes/revision")
	}
}

func TestRecoveryChannelMovedEndpointEstablishesNewIdentity(t *testing.T) {
	doer, fixture := recoveryChannelFixture(t)
	recoveryChannelRead(t, doer)
	channel := doer.Channel
	channel.mu.Lock()
	oldConn, oldIdentity := channel.conn, cloneSeed(channel.seed)
	channel.mu.Unlock()
	moved := cloneSeed(oldIdentity)
	moved.Revision, moved.PeerIP = "2", "198.51.100.10"
	if err := doer.Seeds.Update(SourceCached, seedJSON(t, moved)); err != nil {
		t.Fatal(err)
	}
	// Establish runs while Channel owns its lock. Inspect the transition at that
	// exact seam: the old socket is already closed and still has its old identity.
	channel.Establishment = EstablishFunc(func(ctx context.Context, seed Seed) (net.Conn, func(), error) {
		if channel.conn != nil || recoverySeedDigest(channel.seed) != recoverySeedDigest(oldIdentity) {
			t.Fatal("old connection was relabeled before replacement establishment")
		}
		select {
		case <-oldConn.(*loopConn).closed:
		default:
			t.Fatal("new endpoint establishment began before old socket closed")
		}
		if recoverySeedDigest(seed) != recoverySeedDigest(moved) {
			t.Fatal("replacement establishment received the old endpoint")
		}
		return fixture.Establish(ctx, seed)
	})
	recoveryChannelRead(t, doer)
	channel.mu.Lock()
	newConn, newIdentity := channel.conn, cloneSeed(channel.seed)
	channel.mu.Unlock()
	if fixture.dialCount() != 2 || newConn == oldConn || recoverySeedDigest(newIdentity) != recoverySeedDigest(moved) {
		t.Fatal("moved endpoint did not acquire a distinct established connection")
	}
}

func TestRecoveryChannelServiceSeedExactAllowlist(t *testing.T) {
	doer, fixture := recoveryChannelFixture(t)
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/mobile/v1/service-seed"},
		{http.MethodDelete, "/api/mobile/v1/service-seed"},
		{http.MethodHead, "/api/mobile/v1/service-seed"},
		{http.MethodGet, "/api/mobile/v1/service-seed/"},
		{http.MethodGet, "/api/mobile/v1/service-seed/extra"},
		{http.MethodGet, "/api/mobile/v1/service-seeds"},
		{http.MethodGet, "/api/mobile/v1/Service-seed"},
	} {
		request, err := newTestRequest(route.method, testOrigin+route.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if response, err := doer.Do(request); !errors.Is(err, ErrPathRejected) && !errors.Is(err, ErrRequestRejected) {
			if response != nil {
				_ = response.Body.Close()
			}
			t.Fatalf("variant accepted: %s %s: %v", route.method, route.path, err)
		}
	}
	if fixture.dialCount() != 0 {
		t.Fatal("rejected service-seed variants reached the channel")
	}
	request, err := newTestRequest(http.MethodGet, testOrigin+"/api/mobile/v1/service-seed", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := doer.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	received, _, _, invalid := fixture.snapshot()
	if len(received) != 1 || received[0].Method != http.MethodGet || received[0].Path != "/api/mobile/v1/service-seed" || fixture.dialCount() != 1 {
		t.Fatal("exact GET service-seed route did not reach the service fixture")
	}
	for _, err := range invalid {
		if err != nil {
			t.Fatalf("service wire self-check rejected exact route: %v", err)
		}
	}
}
