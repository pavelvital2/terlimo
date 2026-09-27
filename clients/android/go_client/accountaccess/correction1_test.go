package accountaccess

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// correction1 (review /home/pavel/Laptop_DeepSeek/review-034core-20260921/REPORT.md):
// focused regressions for C1 (non-200 invariant), C2 (terminal receipt must not block
// later legitimate sync), C3 (strict decode) and C4 (subject replacement generation).

func TestCorrection1MalformedNon200CatalogIsNeverAuthoritative(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"html502", http.StatusBadGateway, "<html><body>502 Bad Gateway</body></html>"},
		{"empty502", http.StatusBadGateway, ""},
		{"malformedErrorJson502", http.StatusBadGateway, `{"status":"error","code":`},
		{"nonErrorJson503", http.StatusServiceUnavailable, `{"status":"ok","gateways":[]}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := &fakeServer{gatewayStatus: testCase.status, gatewayBody: testCase.body}
			subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
			coordinator, _ := newTestCoordinator(t, fake, &subject)
			result, err := coordinator.RefreshCatalog(context.Background())
			if err == nil {
				t.Fatalf("malformed non-200 body must be an explicit error, got result %+v", result)
			}
			if result.Applied || result.AuthoritativeEmpty || coordinator.LastGood() != nil {
				t.Fatalf("malformed non-200 adopted as authoritative: %+v", result)
			}
		})
	}
}

func TestCorrection1MalformedNon200KeepsLastGood(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("4", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayStatus = http.StatusBadGateway
	fake.gatewayBody = "<html><body>502 Bad Gateway</body></html>"
	result, err := coordinator.RefreshCatalog(context.Background())
	if err == nil || result.Applied {
		t.Fatalf("malformed 502 must not apply: %+v %v", result, err)
	}
	if coordinator.LastGood() == nil || len(coordinator.LastGood().Gateways) != 1 {
		t.Fatal("last-good catalog lost after malformed 502")
	}
}

func TestCorrection1MalformedNon200MeIsExplicitError(t *testing.T) {
	fake := &fakeServer{meBody: "<html><body>502 Bad Gateway</body></html>"}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	result, err := coordinator.Refresh(context.Background())
	if err == nil || result.Projection != nil || result.State == StateApplied {
		t.Fatalf("malformed non-200 /me must fail without emission: %+v %v", result, err)
	}
}

func TestCorrection1TerminalReceiptAllowsLaterLegitimateSync(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("7", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.syncResponse = syncBody("op-1", "applied")
	first, err := coordinator.SyncAccess(context.Background())
	if err != nil || first.Receipt == nil {
		t.Fatalf("first sync: %+v %v", first, err)
	}
	firstKey := fake.lastSyncKey

	fake.gatewayBody = catalogBody("9", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.SyncAccess(context.Background())
	if err != nil || second.Receipt == nil {
		t.Fatalf("later legitimate sync must start a new effect: %+v %v", second, err)
	}
	if fake.syncCalls != 2 || fake.lastSyncKey == firstKey {
		t.Fatalf("expected a new operation with a fresh key: calls=%d key=%q first=%q",
			fake.syncCalls, fake.lastSyncKey, firstKey)
	}
	if !strings.Contains(fake.lastSyncBody, `"catalog_revision":"9"`) {
		t.Fatalf("new operation must carry the new body: %s", fake.lastSyncBody)
	}
}

type lossySyncServer struct {
	mu     sync.Mutex
	fail   bool
	keys   []string
	bodies []string
}

func (l *lossySyncServer) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/access/sync" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(request.Body)
		l.mu.Lock()
		l.keys = append(l.keys, request.Header.Get("Idempotency-Key"))
		l.bodies = append(l.bodies, string(raw))
		fail := l.fail
		l.mu.Unlock()
		if fail {
			// Advertise a longer body and cut the response: the client observes a
			// transport error after the server has accepted the request.
			writer.Header().Set("Content-Length", "4096")
			_, _ = io.WriteString(writer, `{"request_id":"0123456789abcdef0123456789abcdef",`)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, syncBody("op-1", "applied"))
	})
}

func TestCorrection1LostResponseResumesSameKeyWithoutMe(t *testing.T) {
	lossy := &lossySyncServer{fail: true}
	server := httptest.NewServer(lossy.handler())
	defer server.Close()
	store := &MemoryReceiptStore{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	options := Options{
		Client:    &Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: StaticToken("bearer")},
		AttemptID: "attempt",
		Store:     store,
		Subject:   func() Subject { return subject },
	}
	body := AccessSyncRequest{CatalogRevision: "7", BindingRevision: "1"}
	coordinator, err := NewCoordinator(options)
	if err != nil {
		t.Fatal(err)
	}
	// A direct send path with a durable body: seed the uncertain operation exactly as
	// a first attempt would persist it before the network send.
	pending := Receipt{Subject: subject, Key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BodyDigest: mustDigest(t, body), Request: body}
	if _, err := coordinator.sendSync(context.Background(), pending); err == nil {
		t.Fatal("lost response must surface as a transport error")
	}
	if store.Value == nil || store.Value.Response != nil {
		t.Fatalf("uncertain operation must stay durable: %+v", store.Value)
	}

	// Restart: a fresh coordinator over the same store resumes the SAME key/body with no
	// /me fetch needed (the durable body already carries the fences).
	lossy.mu.Lock()
	lossy.fail = false
	lossy.mu.Unlock()
	restarted, err := NewCoordinator(options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := restarted.SyncAccess(context.Background())
	if err != nil || result.Receipt == nil {
		t.Fatalf("restart resume: %+v %v", result, err)
	}
	lossy.mu.Lock()
	defer lossy.mu.Unlock()
	if len(lossy.keys) != 2 || lossy.keys[0] != lossy.keys[1] {
		t.Fatalf("lost response must resume the same idempotency key: %v", lossy.keys)
	}
	if lossy.bodies[0] != lossy.bodies[1] {
		t.Fatalf("same operation must keep the same canonical body: %s vs %s", lossy.bodies[0], lossy.bodies[1])
	}
}

func mustDigest(t *testing.T, body AccessSyncRequest) string {
	t.Helper()
	digest, err := body.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestCorrection1NewOperationRequiresResolvedMe(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	if _, err := coordinator.SyncAccess(context.Background()); err != ErrMeRequired {
		t.Fatalf("sync before /me must be an explicit ordering error, got %v", err)
	}
	fake.meBody = meBody("7", nil, "not_started")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.SyncAccess(context.Background()); err != ErrNoBinding {
		t.Fatalf("null binding must not start sync, got %v", err)
	}
	if fake.syncCalls != 0 {
		t.Fatalf("no sync request may be sent before /me resolves: %d", fake.syncCalls)
	}
}

func TestCorrection1ReceiptForOtherSubjectIsNotReplayed(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, store := newTestCoordinator(t, fake, &subject)
	otherResponse := AccessSyncResponse{SchemaVersion: "1.0", Status: "ok", OperationID: "op-other",
		AccessApplicationState: "applied"}
	store.Value = &Receipt{Subject: Subject{AccountRef: "acc-2", InstallationID: "inst-1"},
		Key: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", BodyDigest: "other", Response: &otherResponse}
	reloaded, err := NewCoordinator(Options{
		Client:    coordinator.opts.Client,
		AttemptID: "attempt",
		Store:     store,
		Subject:   func() Subject { return subject },
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := reloaded.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("7", 1)
	if _, err := reloaded.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.syncResponse = syncBody("op-1", "applied")
	result, err := reloaded.SyncAccess(context.Background())
	if err != nil || result.Receipt == nil {
		t.Fatalf("new subject: %+v %v", result, err)
	}
	if fake.syncCalls != 1 || fake.lastSyncKey == "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("old subject receipt must not be replayed: calls=%d key=%q", fake.syncCalls, fake.lastSyncKey)
	}
}

func TestCorrection1SubjectReplacementLinksGenerationAndKeepsNewBaseline(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("12", &binding, "active")
	first, err := coordinator.Refresh(context.Background())
	if err != nil || first.Projection == nil {
		t.Fatalf("first refresh: %+v %v", first, err)
	}
	if first.Projection.Payload()["session_generation"] != "1" ||
		first.Projection.Payload()["previous_session_generation"] != nil {
		t.Fatalf("first pair wrong: %v", first.Projection.Payload())
	}

	subject = Subject{AccountRef: "acc-2", InstallationID: "inst-1"}
	fake.meBody = meBody("11", &binding, "active")
	replaced, err := coordinator.Refresh(context.Background())
	if err != nil || replaced.Projection == nil {
		t.Fatalf("subject replacement must emit a linked generation: %+v %v", replaced, err)
	}
	payload := replaced.Projection.Payload()
	if payload["session_generation"] != "2" || payload["previous_session_generation"] != "1" {
		t.Fatalf("subject replacement chain wrong: %v", payload)
	}

	fake.meBody = meBody("10", &binding, "active")
	stale, err := coordinator.Refresh(context.Background())
	if err != nil || stale.Rejection != "STALE" {
		t.Fatalf("new subject baseline must be kept across bearer refresh: %+v %v", stale, err)
	}
}

func TestCorrection1PendingReceiptSurvivesRefreshAndSessionRotation(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, _ := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.gatewayBody = catalogBody("7", 1)
	if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.syncResponse = syncBody("op-1", "pending")
	first, err := coordinator.SyncAccess(context.Background())
	if err != nil || first.Receipt == nil {
		t.Fatalf("pending sync: %+v %v", first, err)
	}
	key := fake.lastSyncKey
	// Bearer refresh and session rotation must not erase the uncertain operation.
	coordinator.NewSession()
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	replay, err := coordinator.SyncAccess(context.Background())
	if err != nil || !replay.Replayed || fake.syncCalls != 1 || fake.lastSyncKey != key {
		t.Fatalf("uncertain operation was discarded by refresh/rotation: %+v calls=%d key=%q", replay, fake.syncCalls, fake.lastSyncKey)
	}
}

func TestCorrection1MalformedNon200OperationIsExplicitError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(writer, "<html>502</html>")
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: StaticToken("bearer")}
	response, apiError, err := client.GetOperation(context.Background(), "op-1")
	if err == nil || apiError != nil {
		t.Fatalf("malformed non-200 operation must be an explicit error: %+v %+v %v", response, apiError, err)
	}
}

func TestCorrection1MeStrictEnumBoundsAndManagementOnly(t *testing.T) {
	binding := "1"
	base := meBody("7", &binding, "active")
	mutations := map[string]string{
		"accountEnum":       strings.Replace(base, `"account_state":"ACTIVE_TRIAL"`, `"account_state":"BOGUS"`, 1),
		"bindingEnum":       strings.Replace(base, `"binding_status":"active"`, `"binding_status":"bogus"`, 1),
		"entitlementType":   strings.Replace(base, `"type":"trial"`, `"type":"gold"`, 1),
		"entitlementStatus": strings.Replace(base, `"status":"active"`, `"status":"bogus"`, 1),
		"deviceLimit":       strings.Replace(base, `"effective_device_limit":2`, `"effective_device_limit":101`, 1),
		"slotsUsed":         strings.Replace(base, `"slots_used":1`, `"slots_used":-1`, 1),
		"validFromOffset":   strings.Replace(base, `"valid_from":"2026-09-21T09:00:00Z"`, `"valid_from":"2026-09-21T09:00:00+00:00"`, 1),
		"longSourceRef": strings.Replace(base, `"perpetual_commercial":false`,
			`"perpetual_commercial":false,"source_ref":"`+strings.Repeat("x", 129)+`"`, 1),
		"managementOnly": strings.Replace(base, `"management_only":false`, `"management_only":true`, 1),
	}
	for name, payload := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMeStrict([]byte(payload)); err == nil {
				t.Fatalf("strict /me accepted invalid payload")
			}
		})
	}
	// The reviewer note "never the subject revision" describes the SOURCE of
	// binding_revision (the binding generation), not a mathematical inequality of
	// independent counters: equal values are valid and must decode.
	equal := "7"
	if _, err := DecodeMeStrict([]byte(meBody("7", &equal, "active"))); err != nil {
		t.Fatalf("binding_revision may equal the subject revision as independent counters: %v", err)
	}
}

func TestCorrection1CatalogStrictNodeFields(t *testing.T) {
	base := catalogBody("4", 1)
	mutations := map[string]string{
		"missingName":         strings.Replace(base, `"name":"Synthetic 0"`, `"name":""`, 1),
		"duplicateCapability": strings.Replace(base, `"capabilities":["managed"]`, `"capabilities":["managed","managed"]`, 1),
		"targetWorkersZero":   strings.Replace(base, `"target_workers":36`, `"target_workers":0`, 1),
		"probeKind":           strings.Replace(base, `"target_workers":36`, `"target_workers":36,"probe":{"kind":"udp","host":"h","port":1}`, 1),
		"probePort":           strings.Replace(base, `"target_workers":36`, `"target_workers":36,"probe":{"kind":"tcp_connect","host":"h","port":0}`, 1),
		"tooManyVKHashes": strings.Replace(base, `"generation":"1"`,
			`"generation":"1","vk_hashes":["1","2","3","4","5","6","7","8","9"]`, 1),
	}
	for name, payload := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCatalogStrict([]byte(payload)); err == nil {
				t.Fatalf("strict catalog accepted invalid node")
			}
		})
	}
}
