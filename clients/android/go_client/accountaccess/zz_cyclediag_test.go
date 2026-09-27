package accountaccess

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type diagDoerFunc func(*http.Request) (*http.Response, error)

func (f diagDoerFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

type diagObservation struct {
	path   string
	status int
}

// diagRecorder captures the OnStage chronology and enforces the fixed numeric contract:
// elapsed non-decreasing, UTC in the 13-digit range.
type diagRecorder struct {
	t        *testing.T
	stages   []string
	previous int64
}

func newDiagRecorder(t *testing.T) *diagRecorder {
	t.Helper()
	return &diagRecorder{t: t, previous: -1}
}

func (r *diagRecorder) onStage(token string, elapsedMS, utcMS int64) {
	r.t.Helper()
	if elapsedMS < r.previous {
		r.t.Fatalf("cycle stage elapsed not monotonic: %d after %d (%s)", elapsedMS, r.previous, token)
	}
	if utcMS < 1_000_000_000_000 || utcMS > 9_999_999_999_999 {
		r.t.Fatalf("cycle stage UTC outside the 13-digit contract: %d (%s)", utcMS, token)
	}
	r.previous = elapsedMS
	r.stages = append(r.stages, token)
}

// A successful cycle must mark the /me return, the guarded emit and the pre-refresh
// boundary in exactly that order; nothing else may enter the vocabulary.
func TestRunnerCycleStageSequenceForSuccessfulCycle(t *testing.T) {
	fixture, server := newRunnerFixture(t, true)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	recorder := newDiagRecorder(t)
	runner.config.OnStage = recorder.onStage

	if _, _, err := runner.cycle(context.Background()); err != nil {
		t.Fatalf("successful cycle: %v", err)
	}
	want := []string{cycleStageAfterMeRead, cycleStageEmitBegin, cycleStageEmitEndOK, cycleStageGWRefreshBegin}
	if !equalStrings(recorder.stages, want) {
		t.Fatalf("successful cycle stages: got %v want %v", recorder.stages, want)
	}
}

// The pending (409) path must additionally mark the pending-catalog refresh, so an
// observed pending retry is distinguishable from a stall before the first catalog read.
func TestRunnerCycleStageSequenceForPendingCatalog(t *testing.T) {
	fixture, server := newRunnerFixture(t, true)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	fixture.mu.Lock()
	fixture.gatewayStatus = http.StatusConflict
	fixture.gatewayBody = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z",` +
		`"schema_version":"1.0","status":"error","code":"ACCESS_SYNC_PENDING","retryable":true,` +
		`"retry_after_ms":1500,"details":{"catalog_revision":"5","binding_revision":"1"}}`
	fixture.mu.Unlock()
	recorder := newDiagRecorder(t)
	runner.config.OnStage = recorder.onStage

	catalog, browse, err := runner.cycle(context.Background())
	if err == nil || catalog != nil || browse != nil {
		t.Fatalf("pending cycle wrong: catalog=%v browse=%v err=%v", catalog, browse, err)
	}
	want := []string{cycleStageAfterMeRead, cycleStageEmitBegin, cycleStageEmitEndOK,
		cycleStageGWRefreshBegin, cycleStageGWPendingRefresh}
	if !equalStrings(recorder.stages, want) {
		t.Fatalf("pending cycle stages: got %v want %v", recorder.stages, want)
	}
}

// A canceled emit context must mark EMIT_END_CANCEL, never OK or ERR.
func TestRunnerCycleStageMarksCanceledEmit(t *testing.T) {
	fixture, server := newRunnerFixture(t, true)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	runner.config.Emit = func(context.Context, map[string]any) error {
		cancel()
		return context.Canceled
	}
	recorder := newDiagRecorder(t)
	runner.config.OnStage = recorder.onStage

	if _, _, err := runner.cycle(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled emit must return the context error: %v", err)
	}
	want := []string{cycleStageAfterMeRead, cycleStageEmitBegin, cycleStageEmitEndCancel}
	if !equalStrings(recorder.stages, want) {
		t.Fatalf("canceled emit stages: got %v want %v", recorder.stages, want)
	}
}

// The client hooks must observe the /gateways request boundary and map every HTTP
// status class to the fixed token; a transport failure before any status is TRANSPORT.
func TestClientGatewayHooksObserveRequestAndStatusClasses(t *testing.T) {
	status := http.StatusOK
	transportDown := errors.New("transport down")
	fail := false
	client := &Client{
		BaseURL: "https://example.invalid/api/mobile/v1",
		HTTP: diagDoerFunc(func(*http.Request) (*http.Response, error) {
			if fail {
				return nil, transportDown
			}
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Header:     make(http.Header),
			}, nil
		}),
		Tokens: StaticToken("bearer"),
	}
	var requests, responses []diagObservation
	client.OnRequest = func(path string, s int) { requests = append(requests, diagObservation{path, s}) }
	client.OnResponse = func(path string, s int) { responses = append(responses, diagObservation{path, s}) }

	for _, code := range []int{200, 204, 301, 409, 503} {
		status = code
		if _, _, err := client.requestWith(context.Background(), http.MethodGet, "/gateways", nil, "", true); err != nil {
			t.Fatalf("request status %d: %v", code, err)
		}
	}
	fail = true
	if _, _, err := client.requestWith(context.Background(), http.MethodGet, "/gateways", nil, "", true); !errors.Is(err, transportDown) {
		t.Fatalf("transport error must surface: %v", err)
	}
	// The hooks are general and secret-free; the caller filters by path.
	fail = false
	status = http.StatusOK
	if _, _, err := client.requestWith(context.Background(), http.MethodGet, "/me", nil, "", true); err != nil {
		t.Fatalf("me request: %v", err)
	}

	wantRequests := []diagObservation{{"/gateways", 0}, {"/gateways", 0}, {"/gateways", 0},
		{"/gateways", 0}, {"/gateways", 0}, {"/gateways", 0}, {"/me", 0}}
	wantResponses := []diagObservation{{"/gateways", 200}, {"/gateways", 204}, {"/gateways", 301},
		{"/gateways", 409}, {"/gateways", 503}, {"/gateways", 0}, {"/me", 200}}
	if !equalObservations(requests, wantRequests) {
		t.Fatalf("request hooks: got %v want %v", requests, wantRequests)
	}
	if !equalObservations(responses, wantResponses) {
		t.Fatalf("response hooks: got %v want %v", responses, wantResponses)
	}

	for status, want := range map[int]string{
		0:   "GW_REQUEST_END_TRANSPORT",
		100: "GW_REQUEST_END_OTHER",
		200: "GW_REQUEST_END_2XX",
		204: "GW_REQUEST_END_2XX",
		301: "GW_REQUEST_END_OTHER",
		400: "GW_REQUEST_END_4XX",
		409: "GW_REQUEST_END_4XX",
		500: "GW_REQUEST_END_5XX",
		503: "GW_REQUEST_END_5XX",
		600: "GW_REQUEST_END_OTHER",
	} {
		if got := GatewayRequestStage(status); got != want {
			t.Fatalf("status %d mapped to %q, want %q", status, got, want)
		}
	}
	for status, want := range map[int]string{
		0:   "ME_RESPONSE_TRANSPORT",
		100: "ME_RESPONSE_OTHER",
		200: "ME_RESPONSE_2XX",
		204: "ME_RESPONSE_2XX",
		301: "ME_RESPONSE_OTHER",
		400: "ME_RESPONSE_4XX",
		401: "ME_RESPONSE_4XX",
		500: "ME_RESPONSE_5XX",
		503: "ME_RESPONSE_5XX",
		600: "ME_RESPONSE_OTHER",
	} {
		if got := MeResponseStage(status); got != want {
			t.Fatalf("me status %d mapped to %q, want %q", status, got, want)
		}
	}
}

// The production wiring (terlimo_mobile.go) maps the /me wire response to the fixed
// ME_RESPONSE_* boundary class and the /gateways response to GW_REQUEST_END_*, and the
// runner's own cyclestage sequence (asserted above) stays unchanged: ME_RESPONSE comes
// from the client hook at the wire READ, AFTER_ME_READ from the runner after Refresh.
func TestClientMeResponseHookEmitsWireBoundaryStage(t *testing.T) {
	status := http.StatusOK
	fail := false
	client := &Client{
		BaseURL: "https://example.invalid/api/mobile/v1",
		HTTP: diagDoerFunc(func(*http.Request) (*http.Response, error) {
			if fail {
				return nil, errors.New("transport down")
			}
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Header:     make(http.Header),
			}, nil
		}),
		Tokens: StaticToken("bearer"),
	}
	var stages []string
	client.OnResponse = func(path string, s int) {
		switch path {
		case "/gateways":
			stages = append(stages, GatewayRequestStage(s))
		case "/me":
			stages = append(stages, MeResponseStage(s))
		}
	}
	for _, code := range []int{200, 301, 409, 503} {
		status = code
		if _, _, err := client.requestWith(context.Background(), http.MethodGet, "/me", nil, "", true); err != nil {
			t.Fatalf("me status %d: %v", code, err)
		}
	}
	fail = true
	if _, _, err := client.requestWith(context.Background(), http.MethodGet, "/me", nil, "", true); err == nil {
		t.Fatal("transport failure must surface")
	}
	want := []string{"ME_RESPONSE_2XX", "ME_RESPONSE_OTHER", "ME_RESPONSE_4XX",
		"ME_RESPONSE_5XX", "ME_RESPONSE_TRANSPORT"}
	if !equalStrings(stages, want) {
		t.Fatalf("me boundary stages: got %v want %v", stages, want)
	}
}

func equalObservations(got, want []diagObservation) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
