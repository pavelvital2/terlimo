package servicechannel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"wg-turn-client/accountaccess"

	"wg-turn-client/wlwire"
)

const testOrigin = "https://mobile.example.test"

func TestDoerRejectsForeignOriginsAndMethods(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, nil, nil)}
	})
	doer := fixtureDoer(t, fixture)
	cases := []struct {
		name   string
		method string
		target string
	}{
		{"foreign host", "GET", "https://evil.test/api/mobile/v1/me"},
		{"foreign scheme", "GET", "http://mobile.example.test/api/mobile/v1/me"},
		{"foreign port", "GET", "https://mobile.example.test:8443/api/mobile/v1/me"},
		{"userinfo", "GET", "https://user@mobile.example.test/api/mobile/v1/me"},
		{"fragment", "GET", "https://mobile.example.test/api/mobile/v1/me#x"},
		{"unsupported method", "DELETE", "https://mobile.example.test/api/mobile/v1/me"},
		{"head method", "HEAD", "https://mobile.example.test/api/mobile/v1/me"},
	}
	for _, testCase := range cases {
		request, err := newTestRequest(testCase.method, testCase.target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := doer.Do(request); err == nil {
			t.Fatalf("%s: accepted", testCase.name)
		}
	}
	if fixture.dialCount() != 0 {
		t.Fatalf("rejected requests reached the channel: %d dials", fixture.dialCount())
	}
}

func TestDoerAllowlist(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, nil, nil)}
	})
	doer := fixtureDoer(t, fixture)

	accepted := []struct {
		method string
		path   string
	}{
		{"POST", "/api/mobile/v1/auth/challenge"},
		{"POST", "/api/mobile/v1/auth/session"},
		{"POST", "/api/mobile/v1/installations"},
		{"GET", "/api/mobile/v1/me"},
		{"GET", "/api/mobile/v1/gateways"},
		{"POST", "/api/mobile/v1/access/sync"},
		{"GET", "/api/mobile/v1/operations/01234567-89ab-cdef-0123-456789abcdef"},
		{"POST", "/api/mobile/v1/onboarding/intents"},
		{"POST", "/api/mobile/v1/registration/telegram/link"},
		{"POST", "/api/mobile/v1/trial/activate"},
		{"GET", "/api/mobile/v1/plans"},
		{"GET", "/api/mobile/v1/usage"},
		{"POST", "/api/mobile/v1/quotes"},
		{"POST", "/api/mobile/v1/payments"},
		{"GET", "/api/mobile/v1/payments/pay-1"},
		{"POST", "/api/mobile/v1/payments/pay-1/checkout-session"},
	}
	for _, item := range accepted {
		request, err := newTestRequest(item.method, testOrigin+item.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := doer.Do(request); err != nil {
			t.Fatalf("%s %s rejected: %v", item.method, item.path, err)
		}
	}
	received, _, _, invalid := fixture.snapshot()
	if len(received) != len(accepted) {
		t.Fatalf("expected %d recorded requests, got %d", len(accepted), len(received))
	}
	for index, item := range invalid {
		if item != nil {
			t.Fatalf("recorded request %d invalid: %v", index, item)
		}
	}

	rejected := []struct {
		name   string
		method string
		target string
	}{
		{"wrong method", "GET", testOrigin + "/api/mobile/v1/auth/challenge"},
		{"wrong get method", "POST", testOrigin + "/api/mobile/v1/me"},
		{"registration link wrong method", "GET", testOrigin + "/api/mobile/v1/registration/telegram/link"},
		{"unknown path", "GET", testOrigin + "/api/mobile/v1/admin"},
		{"trailing slash", "GET", testOrigin + "/api/mobile/v1/me/"},
		{"non uuid operation", "GET", testOrigin + "/api/mobile/v1/operations/abc"},
		{"dot segment", "GET", testOrigin + "/api/mobile/v1/me/../admin"},
		{"double slash", "GET", testOrigin + "/api/mobile/v1//me"},
		{"percent escape", "GET", testOrigin + "/api/mobile/v1/%6de"},
		{"oversized query", "GET", testOrigin + "/api/mobile/v1/me?" + strings.Repeat("q", maxPathQuery)},
		{"plans wrong method", "POST", testOrigin + "/api/mobile/v1/plans"},
		{"quotes wrong method", "GET", testOrigin + "/api/mobile/v1/quotes"},
		{"payments wrong method", "GET", testOrigin + "/api/mobile/v1/payments"},
		{"checkout wrong method", "GET", testOrigin + "/api/mobile/v1/payments/pay-1/checkout-session"},
		{"empty payment id", "GET", testOrigin + "/api/mobile/v1/payments/"},
		{"nested payment id", "GET", testOrigin + "/api/mobile/v1/payments/a/b"},
		{"payment traversal", "GET", testOrigin + "/api/mobile/v1/payments/.."},
		{"checkout traversal", "POST", testOrigin + "/api/mobile/v1/payments/../checkout-session"},
		{"checkout nested id", "POST", testOrigin + "/api/mobile/v1/payments/a/b/checkout-session"},
	}
	for _, testCase := range rejected {
		request, err := newTestRequest(testCase.method, testCase.target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := doer.Do(request); !errors.Is(err, ErrPathRejected) {
			t.Fatalf("%s: expected ErrPathRejected, got %v", testCase.name, err)
		}
	}
	rawQuery, err := newTestRequest("GET", testOrigin+"/api/mobile/v1/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	rawQuery.URL.RawQuery = "q=\r\nInjected: 1"
	if _, err := doer.Do(rawQuery); !errors.Is(err, ErrPathRejected) {
		t.Fatalf("literal CRLF query accepted: %v", err)
	}
}

// TestDoerPaymentAllowlistMatrix pins the S5 payment family allowlist: the five
// contract method/path pairs, the dynamic /payments/{id} segment shape and every
// traversal/percent/foreign-path rejection.
func TestDoerPaymentAllowlistMatrix(t *testing.T) {
	accepted := []struct {
		method string
		path   string
		class  string
	}{
		{"GET", "/api/mobile/v1/plans", "PLANS"},
		{"GET", "/api/mobile/v1/usage", "USAGE"},
		{"POST", "/api/mobile/v1/quotes", "QUOTES"},
		{"POST", "/api/mobile/v1/payments", "PAYMENTS"},
		{"GET", "/api/mobile/v1/payments/pay_1", "PAYMENTS"},
		{"GET", "/api/mobile/v1/payments/A.b~c-1", "PAYMENTS"},
		{"GET", "/api/mobile/v1/payments/" + strings.Repeat("a", 128), "PAYMENTS"},
		{"POST", "/api/mobile/v1/payments/pay_1/checkout-session", "CHECKOUT"},
	}
	for _, item := range accepted {
		if !pathAllowed(item.method, item.path) {
			t.Fatalf("allowlist rejected %s %s", item.method, item.path)
		}
		if class := requestClassForPath(item.path); class != item.class {
			t.Fatalf("class(%s)=%s want %s", item.path, class, item.class)
		}
	}

	rejected := []struct {
		method string
		path   string
	}{
		{"POST", "/api/mobile/v1/plans"},
		{"GET", "/api/mobile/v1/quotes"},
		{"GET", "/api/mobile/v1/payments"},
		{"DELETE", "/api/mobile/v1/payments/pay_1"},
		{"GET", "/api/mobile/v1/payments/"},
		{"GET", "/api/mobile/v1/payments/."},
		{"GET", "/api/mobile/v1/payments/.."},
		{"GET", "/api/mobile/v1/payments/a..b"},
		{"GET", "/api/mobile/v1/payments/pay-1%2Fme"},
		{"GET", "/api/mobile/v1/payments/a/b"},
		{"GET", "/api/mobile/v1/payments/" + strings.Repeat("a", 129)},
		{"GET", "/api/mobile/v1/payments/pay-1/checkout-session"},
		{"POST", "/api/mobile/v1/payments/pay-1/checkout-session/extra"},
		{"POST", "/api/mobile/v1/payments//checkout-session"},
		{"POST", "/api/mobile/v1/admin/payments/pay-1/checkout-session"},
		{"GET", "/api/mobile/v1/me/payments/pay-1"},
	}
	for _, item := range rejected {
		if pathAllowed(item.method, item.path) {
			t.Fatalf("allowlist accepted %s %s", item.method, item.path)
		}
	}
}

// TestDoerForwardsHostIdempotencyKeyVerbatimOnRetry proves the captured service frame
// carries the host-owned key byte-for-byte on both attempts of a lost-response retry;
// the doer has no code path that generates or rotates a key.
func TestDoerForwardsHostIdempotencyKeyVerbatimOnRetry(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, map[string]string{"Content-Type": "application/json"}, []byte(`{"ok":true}`))}
	})
	doer := fixtureDoer(t, fixture)
	const hostKey = "host-key-00000000000000000000000042"
	for attempt := 0; attempt < 2; attempt++ {
		request, err := newTestRequest("POST", testOrigin+"/api/mobile/v1/payments", []byte(`{"quote_id":"q-1"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", hostKey)
		if _, err := doer.Do(request); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	received, _, _, invalid := fixture.snapshot()
	if len(received) != 2 {
		t.Fatalf("expected 2 captured request frames, got %d", len(received))
	}
	for index, frame := range received {
		if frame.Path != "/api/mobile/v1/payments" || frame.Headers["Idempotency-Key"] != hostKey {
			t.Fatalf("captured frame %d changed the host key: %+v", index, frame.Headers)
		}
		if invalid[index] != nil {
			t.Fatalf("captured frame %d invalid: %v", index, invalid[index])
		}
	}
}

func TestDoerHeaderRules(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, nil, nil)}
	})
	doer := fixtureDoer(t, fixture)
	request, err := newTestRequest("POST", testOrigin+"/api/mobile/v1/access/sync", []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer exact-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "idem-1")
	if _, err := doer.Do(request); err != nil {
		t.Fatal(err)
	}
	received, raw, _, _ := fixture.snapshot()
	if len(received) != 1 {
		t.Fatalf("request not recorded: %d", len(received))
	}
	if received[0].Headers["Authorization"] != "Bearer exact-token" ||
		received[0].Headers["Content-Type"] != "application/json" ||
		received[0].Headers["Idempotency-Key"] != "idem-1" {
		t.Fatalf("headers changed: %+v", received[0].Headers)
	}
	var frame requestFrame
	if err := json.Unmarshal(raw[0], &frame); err != nil {
		t.Fatal(err)
	}
	body, err := base64.RawURLEncoding.Strict().DecodeString(frame.BodyB64)
	if err != nil || string(body) != `{"a":1}` {
		t.Fatalf("body not carried exactly: %q %v", body, err)
	}

	rejected := []struct {
		name  string
		apply func(*http.Request)
	}{
		{"unknown header", func(req *http.Request) { req.Header.Set("X-Unknown", "1") }},
		{"multi value", func(req *http.Request) { req.Header["Authorization"] = []string{"a", "b"} }},
		{"crlf value", func(req *http.Request) { req.Header["Authorization"] = []string{"Bearer a\r\nX: 1"} }},
		{"oversized value", func(req *http.Request) { req.Header.Set("Authorization", strings.Repeat("a", 1025)) }},
	}
	for _, testCase := range rejected {
		next, err := newTestRequest("POST", testOrigin+"/api/mobile/v1/access/sync", nil)
		if err != nil {
			t.Fatal(err)
		}
		testCase.apply(next)
		if _, err := doer.Do(next); !errors.Is(err, ErrRequestRejected) {
			t.Fatalf("%s: expected ErrRequestRejected, got %v", testCase.name, err)
		}
	}
}

func TestDoerBodyBounds(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, nil, nil)}
	})
	doer := fixtureDoer(t, fixture)
	request, err := newTestRequest("POST", testOrigin+"/api/mobile/v1/access/sync", bytes.Repeat([]byte{0x61}, maxRequestBody))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doer.Do(request); err != nil {
		t.Fatalf("64 KiB request rejected: %v", err)
	}
	request, err = newTestRequest("POST", testOrigin+"/api/mobile/v1/access/sync", bytes.Repeat([]byte{0x61}, maxRequestBody+1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doer.Do(request); !errors.Is(err, ErrRequestRejected) {
		t.Fatalf("oversized request accepted: %v", err)
	}
}

func TestDoerResponseMapping(t *testing.T) {
	envelope := []byte(`{"request_id":"x","server_time":"2026-09-22T00:00:00Z","schema_version":"1.0","status":"error","code":"ACCESS_SYNC_PENDING","retryable":true}`)
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		switch frame.Path {
		case "/api/mobile/v1/me":
			return fixtureResponse{body: responseJSON(t, id, 200, map[string]string{"Content-Type": "application/json", "Retry-After": "5"}, []byte(`{"me":true}`))}
		case "/api/mobile/v1/gateways":
			return fixtureResponse{body: responseJSON(t, id, 409, map[string]string{"Content-Type": "application/json"}, envelope)}
		case "/api/mobile/v1/access/sync":
			return fixtureResponse{body: errorJSON(t, id, "SERVICE_UNAVAILABLE", true)}
		case "/api/mobile/v1/operations/01234567-89ab-cdef-0123-456789abcdef":
			return fixtureResponse{body: responseJSON(t, id, 302, map[string]string{"Content-Type": "application/json"}, []byte(`{"moved":true}`))}
		default:
			return fixtureResponse{body: responseJSON(t, id, 200, map[string]string{"Location": "https://evil.test/"}, nil)}
		}
	})
	doer := fixtureDoer(t, fixture)

	request, _ := newTestRequest("GET", testOrigin+"/api/mobile/v1/me", nil)
	response, err := doer.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || string(body) != `{"me":true}` ||
		response.Header.Get("Content-Type") != "application/json" || response.Header.Get("Retry-After") != "5" {
		t.Fatalf("200 mapping changed: %d %q %v", response.StatusCode, body, response.Header)
	}

	request, _ = newTestRequest("GET", testOrigin+"/api/mobile/v1/gateways", nil)
	response, err = doer.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 409 || !bytes.Equal(body, envelope) {
		t.Fatalf("409 passthrough changed: %d %q", response.StatusCode, body)
	}

	request, _ = newTestRequest("POST", testOrigin+"/api/mobile/v1/access/sync", nil)
	_, err = doer.Do(request)
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) || serviceErr.Code != "SERVICE_UNAVAILABLE" || !serviceErr.Retryable {
		t.Fatalf("error frame not classified: %v", err)
	}

	request, _ = newTestRequest("GET", testOrigin+"/api/mobile/v1/operations/01234567-89ab-cdef-0123-456789abcdef", nil)
	response, err = doer.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 302 || string(body) != `{"moved":true}` {
		t.Fatalf("302 was not passed through untouched: %d %q", response.StatusCode, body)
	}
	if received, _, _, _ := fixture.snapshot(); len(received) != 4 || fixture.dialCount() != 2 {
		t.Fatalf("redirect changed bounded request count: %d requests, %d dials", len(received), fixture.dialCount())
	}

	request, _ = newTestRequest("POST", testOrigin+"/api/mobile/v1/installations", nil)
	if _, err := doer.Do(request); !errors.Is(err, ErrResponseRejected) {
		t.Fatalf("redirect header accepted: %v", err)
	}
}

func TestDoerStrictMalformedResponse(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{rawFrames: [][]byte{[]byte("garbage")}}
	})
	doer := fixtureDoer(t, fixture)
	request, _ := newTestRequest("GET", testOrigin+"/api/mobile/v1/me", nil)
	if _, err := doer.Do(request); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("malformed channel reply accepted: %v", err)
	}

	wrongID := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, mustID(t, "ffffffffffffffffffffffffffffffff"), 200, nil, nil)}
	})
	doer = fixtureDoer(t, wrongID)
	var stages []string
	doer.Channel.Observe = func(stage string) { stages = append(stages, stage) }
	doer.Observe = func(stage string) { stages = append(stages, stage) }
	request, _ = newTestRequest("GET", testOrigin+"/api/mobile/v1/me", nil)
	if _, err := doer.Do(request); !errors.Is(err, ErrResponseRejected) {
		t.Fatalf("mismatched request_id accepted: %v", err)
	}
	if !strings.Contains(strings.Join(stages, ","), "FRAME_READ_OK,SERVICE_REPLY_BEGIN,SERVICE_REPLY_FAILED") {
		t.Fatalf("service reply parse boundary missing: %v", stages)
	}
}

func TestDoerRequiresSeed(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, nil, nil)}
	})
	doer, err := NewDoer(testOrigin, NewStore(Config{}), NewChannel(fixture))
	if err != nil {
		t.Fatal(err)
	}
	request, _ := newTestRequest("GET", testOrigin+"/api/mobile/v1/me", nil)
	if _, err := doer.Do(request); !errors.Is(err, ErrSeedMissing) {
		t.Fatalf("missing seed did not fail closed: %v", err)
	}
}

func TestDoerUserHashUpdateKeepsEndpointTarget(t *testing.T) {
	fixture := newFixtureEstablisher(t, func(frame requestFrame, id wlwire.ID) fixtureResponse {
		return fixtureResponse{body: responseJSON(t, id, 200, nil, nil)}
	})
	store := NewStore(Config{Environment: "test"})
	if err := store.Update(SourceBuiltin, seedJSON(t, validSeed())); err != nil {
		t.Fatal(err)
	}
	channel := NewChannel(fixture)
	doer, err := NewDoer(testOrigin, store, channel)
	if err != nil {
		t.Fatal(err)
	}
	userSeed := validSeed()
	userSeed.VKHashes = []string{"rotated-hash"}
	raw, _ := json.Marshal(userSeed)
	if err := store.Update(SourceUser, raw); err != nil {
		t.Fatal(err)
	}
	request, _ := newTestRequest("GET", testOrigin+"/api/mobile/v1/me", nil)
	if _, err := doer.Do(request); err != nil {
		t.Fatal(err)
	}
	if fixture.dialCount() != 1 {
		t.Fatal("rotated user hash did not reach the endpoint")
	}
	if err := store.Update(SourceUser, []byte(`{"version":1,"revision":"1","environment":"test","peer_ip":"203.0.113.5","dtls_port":1,"dtls_spki_sha256":"`+validSeed().DTLSSPKISHA256+`","service_classifier":"synthetic-public-classifier","vk_hashes":["h"],"stream_id":0}`)); !errors.Is(err, ErrSeedEndpoint) {
		t.Fatalf("user seed moved the endpoint: %v", err)
	}
}

// TestDoerAnnouncementsAllowlistMatrix pins the S5 §11 announcements allowlist: the
// list GET, the bounded read-marker POST and every traversal/foreign-path rejection.
func TestDoerAnnouncementsAllowlistMatrix(t *testing.T) {
	accepted := []struct {
		method string
		path   string
		class  string
	}{
		{"GET", "/api/mobile/v1/announcements", "ANNOUNCEMENTS"},
		{"POST", "/api/mobile/v1/announcements/an-1/read", "ANNOUNCEMENT_READ"},
		{"POST", "/api/mobile/v1/announcements/A.b~c-1/read", "ANNOUNCEMENT_READ"},
		{"POST", "/api/mobile/v1/announcements/" + strings.Repeat("a", 128) + "/read", "ANNOUNCEMENT_READ"},
	}
	for _, item := range accepted {
		if !pathAllowed(item.method, item.path) {
			t.Fatalf("allowlist rejected %s %s", item.method, item.path)
		}
		if class := requestClassForPath(item.path); class != item.class {
			t.Fatalf("class(%s)=%s want %s", item.path, class, item.class)
		}
	}

	rejected := []struct {
		method string
		path   string
	}{
		{"POST", "/api/mobile/v1/announcements"},
		{"GET", "/api/mobile/v1/announcements/an-1/read"},
		{"DELETE", "/api/mobile/v1/announcements/an-1/read"},
		{"POST", "/api/mobile/v1/announcements//read"},
		{"POST", "/api/mobile/v1/announcements/./read"},
		{"POST", "/api/mobile/v1/announcements/../read"},
		{"POST", "/api/mobile/v1/announcements/a..b/read"},
		{"POST", "/api/mobile/v1/announcements/a/b/read"},
		{"POST", "/api/mobile/v1/announcements/" + strings.Repeat("a", 129) + "/read"},
		{"POST", "/api/mobile/v1/announcements/an-1"},
		{"POST", "/api/mobile/v1/announcements/an-1/read/extra"},
		{"GET", "/api/mobile/v1/announcements/an-1/checkout-session"},
		{"POST", "/api/mobile/v1/admin/announcements/an-1/read"},
		{"GET", "/api/mobile/v1/me/announcements"},
	}
	for _, item := range rejected {
		if pathAllowed(item.method, item.path) {
			t.Fatalf("allowlist accepted %s %s", item.method, item.path)
		}
	}
}

// TestAnnouncementsPathIDBoundaryParity proves the accountaccess read-marker id grammar
// and the service-channel wire allowlist accept exactly the same set, so a forwardable id
// is never rejected late and a rejected id never reaches the wire.
func TestAnnouncementsPathIDBoundaryParity(t *testing.T) {
	samples := []string{"an-1", "A.b~c-1", strings.Repeat("a", 128), strings.Repeat("a", 129),
		"", ".", "..", "a..b", "an:1", "\u0430\u043d-1", "a%2Fb", "a/b", "a b", "a\\b", "a?b", "a#b"}
	for _, id := range samples {
		if got, want := validAnnouncementPathID(id), accountaccess.ValidAnnouncementPathID(id); got != want {
			t.Fatalf("id %q: servicechannel=%v accountaccess=%v", id, got, want)
		}
	}
}
