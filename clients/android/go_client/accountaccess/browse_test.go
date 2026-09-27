package accountaccess

// Focused tests for the owner-contract display (browse) branch of GET /gateways:
// strict metadata-only decoding, exact union with the credential branch, error
// propagation instead of a false empty list, and a coordinator path that never
// touches last-good, the receipt store or the admission/sync flow.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func browseBody(gatewayCount int) string {
	nodes := make([]string, 0, gatewayCount)
	for index := 0; index < gatewayCount; index++ {
		nodes = append(nodes, fmt.Sprintf(`{"gateway_id":"gw-synth-%d","name":"Synthetic %d","region":"test","country_code":"XX"}`,
			index, index))
	}
	return fmt.Sprintf(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z",
		"schema_version":"1.0","status":"ok","catalog_mode":"browse","valid_until":"2026-09-21T12:10:00Z",
		"issued_at":"2026-09-21T11:55:00Z","gateways":[%s]}`, strings.Join(nodes, ","))
}

type gatewayDoer struct {
	status int
	body   string
	err    error
}

func (d gatewayDoer) Do(*http.Request) (*http.Response, error) {
	if d.err != nil {
		return nil, d.err
	}
	status := d.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(d.body))}, nil
}

func browseClient(body string) *Client {
	return &Client{BaseURL: "https://api.example.test", HTTP: gatewayDoer{body: body},
		Tokens: StaticToken("test-bearer")}
}

func TestBrowseDecodeAcceptsMetadataOnlyCatalog(t *testing.T) {
	browse, err := DecodeBrowseStrict([]byte(browseBody(3)))
	if err != nil {
		t.Fatalf("valid browse rejected: %v", err)
	}
	if browse.CatalogMode != CatalogModeBrowse || len(browse.Gateways) != 3 {
		t.Fatalf("browse projection wrong: %+v", browse)
	}
	if browse.Gateways[2].GatewayID != "gw-synth-2" || browse.Gateways[2].Name != "Synthetic 2" ||
		browse.Gateways[2].Region == nil || *browse.Gateways[2].Region != "test" {
		t.Fatalf("browse node projection wrong: %+v", browse.Gateways[2])
	}

	// Optional region/country_code stay nullable exactly like the credential rows.
	nullable := strings.Replace(browseBody(1), `"region":"test","country_code":"XX"`, `"region":null,"country_code":null`, 1)
	decoded, err := DecodeBrowseStrict([]byte(nullable))
	if err != nil || decoded.Gateways[0].Region != nil || decoded.Gateways[0].CountryCode != nil {
		t.Fatalf("nullable display metadata rejected: %+v %v", decoded, err)
	}

	// The union returns the browse member and no credential catalog.
	union, err := DecodeGatewaysStrict([]byte(browseBody(3)))
	if err != nil || union.Browse == nil || union.Catalog != nil {
		t.Fatalf("union must expose the browse member only: %+v %v", union, err)
	}

	// A real successful empty display catalog is accepted, never synthesized.
	empty, err := DecodeBrowseStrict([]byte(strings.Replace(browseBody(0), `"gateways":[]`, `"gateways":[]`, 1)))
	if err != nil || empty.Gateways == nil || len(empty.Gateways) != 0 {
		t.Fatalf("authoritative empty browse rejected: %+v %v", empty, err)
	}
}

func TestBrowseDecodeRejectsEveryCredentialOrAdmissionField(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
	}{
		{"revision", func(body string) string {
			return strings.Replace(body, `"status":"ok",`, `"status":"ok","revision":"4",`, 1)
		}},
		{"node access", func(body string) string {
			return strings.Replace(body, `"country_code":"XX"}`, `"country_code":"XX","access":{"grant_id":"g","device_ref":"d","password":"p","generation":"1","lease_seq":"1","not_after":"2026-09-21T12:10:00Z"}}`, 1)
		}},
		{"node transport", func(body string) string {
			return strings.Replace(body, `"country_code":"XX"}`, `"country_code":"XX","transport":{"protocol":"wdtt-v17"}}`, 1)
		}},
		{"node probe", func(body string) string {
			return strings.Replace(body, `"country_code":"XX"}`, `"country_code":"XX","probe":null}`, 1)
		}},
		{"node capabilities", func(body string) string {
			return strings.Replace(body, `"country_code":"XX"}`, `"country_code":"XX","capabilities":["managed"]}`, 1)
		}},
		{"node target_workers", func(body string) string {
			return strings.Replace(body, `"country_code":"XX"}`, `"country_code":"XX","target_workers":36}`, 1)
		}},
		{"unknown mode", func(body string) string {
			return strings.Replace(body, `"catalog_mode":"browse"`, `"catalog_mode":"credential"`, 1)
		}},
	}
	for _, tc := range cases {
		mutated := tc.mutate(browseBody(2))
		if _, err := DecodeBrowseStrict([]byte(mutated)); err == nil {
			t.Fatalf("%s: browse decoder accepted a forbidden field", tc.name)
		}
		union, err := DecodeGatewaysStrict([]byte(mutated))
		if err == nil {
			t.Fatalf("%s: union accepted a malformed browse body: %+v", tc.name, union)
		}
		catalog, apiError, err := browseClient(mutated).GetGateways(context.Background())
		if err == nil || apiError != nil || !errors.Is(err, ErrMalformedCatalog) ||
			catalog.Catalog != nil || catalog.Browse != nil {
			t.Fatalf("%s: client must reject the malformed browse branch: %+v %v", tc.name, catalog, err)
		}
	}

	// The same body without catalog_mode stays on the unchanged credential path: it is
	// malformed as a credential catalog, never silently accepted as browse.
	withoutMode := strings.Replace(browseBody(2), `"catalog_mode":"browse",`, "", 1)
	if _, err := DecodeGatewaysStrict([]byte(withoutMode)); err == nil {
		t.Fatal("browse-shaped body without catalog_mode must fail the credential decoder")
	}
	// The credential catalog still decodes exactly as before.
	union, err := DecodeGatewaysStrict([]byte(catalogBody("4", 2)))
	if err != nil || union.Catalog == nil || union.Browse != nil {
		t.Fatalf("credential branch changed: %+v %v", union, err)
	}
}

func TestBrowseTransportAndErrorResponsesNeverBecomeEmpty(t *testing.T) {
	transport := browseClient("")
	transport.HTTP = gatewayDoer{err: errors.New("network down")}
	union, apiError, err := transport.GetGateways(context.Background())
	if err == nil || apiError != nil || union.Browse != nil || union.Catalog != nil {
		t.Fatalf("transport error must stay an error: %+v %v", union, err)
	}

	denied := &Client{BaseURL: "https://api.example.test", Tokens: StaticToken("test-bearer"),
		HTTP: gatewayDoer{status: http.StatusForbidden, body: `{"request_id":"0123456789abcdef0123456789abcdef",
			"server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error",
			"code":"ACCESS_DENIED","retryable":false}`}}
	union, apiError, err = denied.GetGateways(context.Background())
	if err != nil || apiError == nil || apiError.Code != "ACCESS_DENIED" ||
		union.Browse != nil || union.Catalog != nil {
		t.Fatalf("denied response must stay an API error, not an empty browse: %+v %v %v", union, apiError, err)
	}

	garbage := browseClient(`{"status":"ok"`)
	if _, _, err := garbage.GetGateways(context.Background()); err == nil {
		t.Fatal("non-JSON body must not become an empty browse")
	}
}

func TestCoordinatorBrowseNeverTouchesLastGoodOrStore(t *testing.T) {
	fake := &fakeServer{}
	subject := Subject{AccountRef: "acc-1", InstallationID: "inst-1"}
	coordinator, store := newTestCoordinator(t, fake, &subject)
	binding := "1"
	fake.meBody = meBody("7", &binding, "active")
	if _, err := coordinator.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Browse before any credential catalog: display only, no last-good, no store write.
	fake.gatewayBody = browseBody(3)
	first, err := coordinator.RefreshCatalog(context.Background())
	if err != nil || first.Browse == nil || first.Catalog != nil || first.Applied {
		t.Fatalf("browse must return the display branch only: %+v %v", first, err)
	}
	if coordinator.LastGood() != nil {
		t.Fatal("browse must not populate last-good")
	}
	if store.Value != nil {
		t.Fatal("browse must not write the receipt store")
	}
	// Browse alone never authorizes a sync.
	if _, err := coordinator.SyncAccess(context.Background()); err == nil || fake.syncCalls != 0 {
		t.Fatalf("browse must not authorize access/sync: %v calls=%d", err, fake.syncCalls)
	}

	// The credential catalog still applies and becomes the immutable last-good anchor.
	fake.gatewayBody = catalogBody("4", 2)
	verified, err := coordinator.RefreshCatalog(context.Background())
	if err != nil || verified.Catalog == nil || !verified.Applied || verified.Browse != nil {
		t.Fatalf("credential catalog must still apply: %+v %v", verified, err)
	}
	anchor := coordinator.LastGood()
	if anchor == nil || anchor.Revision != "4" {
		t.Fatalf("credential last-good missing: %+v", anchor)
	}

	// A later browse (for example an expired subscription) never replaces it.
	fake.gatewayBody = browseBody(3)
	browse, err := coordinator.RefreshCatalog(context.Background())
	if err != nil || browse.Browse == nil || browse.Catalog != nil {
		t.Fatalf("later browse must not carry the credential catalog: %+v %v", browse, err)
	}
	if coordinator.LastGood() != anchor || anchor.Revision != "4" {
		t.Fatal("browse replaced the retained credential catalog")
	}
	if store.Value != nil {
		t.Fatal("browse wrote the receipt store after credential admission")
	}
}

func TestBrowseRefreshAtKeepsFloorAndValidity(t *testing.T) {
	browse, err := DecodeBrowseStrict([]byte(browseBody(1)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	validUntil, _ := parseUtc(browse.ValidUntil)
	if next := BrowseRefreshAt(browse, now, time.Minute); !next.Equal(validUntil) {
		t.Fatalf("browse refresh must follow valid_until: %v", next)
	}
	if next := BrowseRefreshAt(browse, validUntil.Add(-time.Second), time.Minute); !next.Equal(validUntil.Add(time.Minute - time.Second)) {
		t.Fatalf("floor must bound the wait from now: %v", next)
	}
	expired := browse
	expired.ValidUntil = "2026-09-21T11:00:00Z"
	if next := BrowseRefreshAt(expired, now, 2*time.Minute); !next.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("expired browse validity must fall back to the floor: %v", next)
	}
}

func TestBrowseUnionThroughHTTPServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, browseBody(3))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: StaticToken("test-bearer")}
	union, apiError, err := client.GetGateways(context.Background())
	if err != nil || apiError != nil || union.Browse == nil || len(union.Browse.Gateways) != 3 {
		t.Fatalf("browse through the real HTTP path failed: %+v %v %v", union, apiError, err)
	}
}
