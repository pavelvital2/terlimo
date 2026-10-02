package servicechannel

import (
	"testing"

	"wg-turn-client/wlwire"
)

func TestPaymentContractV2QueryCompatibility(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		if frame.Query != "payment_contract=2" {
			t.Fatalf("query changed: %q", frame.Query)
		}
		return fixtureResponse{body: responseJSON(t, id, 200, nil, nil)}
	})
	doer := fixtureDoer(t, fixture)
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/mobile/v1/plans"},
		{"POST", "/api/mobile/v1/quotes"},
		{"POST", "/api/mobile/v1/payments"},
		{"GET", "/api/mobile/v1/payments/01234567-89ab-cdef-0123-456789abcdef"},
	} {
		request, err := newTestRequest(route.method, testOrigin+route.path+"?payment_contract=2", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := doer.Do(request)
		if err != nil {
			t.Fatalf("query2 %s %s rejected: %v", route.method, route.path, err)
		}
		response.Body.Close()
	}
	received, _, _, invalid := fixture.snapshot()
	if len(received) != 4 {
		t.Fatalf("query2 transport compatibility: %d frames", len(received))
	}
	for _, err := range invalid {
		if err != nil {
			t.Fatalf("query2 frame invalid: %v", err)
		}
	}
}
