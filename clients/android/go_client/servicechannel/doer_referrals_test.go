package servicechannel

import (
	"errors"
	"io"
	"testing"

	"wg-turn-client/wlwire"
)

func TestDoerReferralExactAllowlist(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, nil, nil)}
	})
	doer := fixtureDoer(t, fixture)
	for _, pair := range []struct{ method, path string }{
		{"GET", "/api/mobile/v1/referral"},
		{"POST", "/api/mobile/v1/referral/candidate"},
		{"DELETE", "/api/mobile/v1/referral/candidate"},
	} {
		request, err := newTestRequest(pair.method, testOrigin+pair.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer referral-fixture")
		if pair.method != "GET" {
			request.Header.Set("Idempotency-Key", "original-candidate-key")
		}
		response, err := doer.Do(request)
		if err != nil {
			t.Fatalf("accepted pair rejected: %+v / %v", pair, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	received, _, _, invalid := fixture.snapshot()
	if len(received) != 3 {
		t.Fatalf("accepted referral pairs did not reach authenticated channel: %d", len(received))
	}
	for _, err := range invalid {
		if err != nil {
			t.Fatalf("invalid channel frame: %v", err)
		}
	}
	before := fixture.dialCount()
	for _, pair := range []struct{ method, path string }{
		{"POST", "/api/mobile/v1/referral"},
		{"DELETE", "/api/mobile/v1/referral"},
		{"GET", "/api/mobile/v1/referral/candidate"},
		{"PUT", "/api/mobile/v1/referral/candidate"},
		{"PATCH", "/api/mobile/v1/referral/candidate"},
		{"DELETE", "/api/mobile/v1/referral/candidate/"},
		{"DELETE", "/api/mobile/v1/referral/candidate/other"},
		{"POST", "/api/mobile/v1/referral/candidate/../other"},
		{"POST", "/api/mobile/v1/referral/%63andidate"},
		{"GET", "/api/mobile/v1/referral/"},
		{"GET", "/api/mobile/v1/referral//"},
	} {
		request, err := newTestRequest(pair.method, testOrigin+pair.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := doer.Do(request); !errors.Is(err, ErrPathRejected) && !errors.Is(err, ErrRequestRejected) {
			t.Fatalf("unapproved pair reached channel: %+v / %v", pair, err)
		}
	}
	if fixture.dialCount() != before {
		t.Fatal("rejected referral path opened a channel")
	}
}
