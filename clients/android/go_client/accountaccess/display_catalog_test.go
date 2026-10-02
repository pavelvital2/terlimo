package accountaccess

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type displayDoer func(*http.Request) (*http.Response, error)

func (do displayDoer) Do(req *http.Request) (*http.Response, error) { return do(req) }

func displayResponse(body string, status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body))}
}

func TestDisplayClientUsesExplicitQueryAndRejectsCredentialResponse(t *testing.T) {
	credential := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || req.URL.Path != "/gateways" || req.URL.RawQuery != "view=browse" ||
			req.Header.Get("Authorization") != "Bearer synthetic" {
			t.Errorf("unexpected display request method/path/query/auth: %s %s", req.Method, req.URL)
		}
		if credential {
			_, _ = io.WriteString(w, catalogBody("4", 1))
		} else {
			_, _ = io.WriteString(w, browseBody(3))
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, HTTP: server.Client(), Tokens: StaticToken("synthetic"),
		OnRequest: func(path string, _ int) {
			if path != "/gateways" {
				t.Errorf("route observer changed: %q", path)
			}
		}}
	got, api, err := client.GetDisplayGateways(context.Background())
	if err != nil || api != nil || got.Browse == nil || got.Catalog != nil || len(got.Browse.Gateways) != 3 {
		t.Fatalf("display response: %+v %v %v", got, api, err)
	}
	credential = true
	got, api, err = client.GetDisplayGateways(context.Background())
	if !errors.Is(err, ErrMalformedCatalog) || api != nil || got.Browse != nil || got.Catalog != nil {
		t.Fatalf("explicit browse accepted credential fallback: %+v %v %v", got, api, err)
	}
}

func TestDisplayCoordinatorKeepsCredentialAdmissionAndReceiptState(t *testing.T) {
	for _, outcome := range []string{"browse", "pending", "credential", "canceled", "session", "subject"} {
		t.Run(outcome, func(t *testing.T) {
			binding := "1"
			fake := &fakeServer{meBody: meBody("7", &binding, "active"), gatewayBody: catalogBody("4", 1)}
			subject := Subject{AccountRef: "account", InstallationID: "installation"}
			coordinator, store := newTestCoordinator(t, fake, &subject)
			if _, err := coordinator.Refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := coordinator.RefreshCatalog(context.Background()); err != nil {
				t.Fatal(err)
			}
			retained := coordinator.LastGood()
			tokens := &AdmissionTokens{CatalogRevision: "5", BindingRevision: "1", subject: subject, tick: coordinator.sessionTick}
			receipt := &Receipt{Subject: subject, Key: "existing-pending", Request: AccessSyncRequest{CatalogRevision: "5", BindingRevision: "1"}}
			coordinator.admission, coordinator.receipt, store.Value = tokens, receipt, receipt
			digest, catalogTick := coordinator.lastGoodDigest, coordinator.catalogTick
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			coordinator.opts.Client.HTTP = displayDoer(func(req *http.Request) (*http.Response, error) {
				if req.URL.RawQuery != "view=browse" {
					t.Fatalf("missing display query: %s", req.URL)
				}
				switch outcome {
				case "pending":
					return displayResponse(`{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-21T12:00:00Z","schema_version":"1.0","status":"error","code":"ACCESS_SYNC_PENDING","retryable":true,"details":{"catalog_revision":"999","binding_revision":"1"}}`, 409), nil
				case "credential":
					return displayResponse(catalogBody("999", 0), 200), nil
				case "canceled":
					cancel() // A transport that still returns success must not publish late.
				case "session":
					coordinator.NewSession()
				case "subject":
					subject.AccountRef = "replacement"
				}
				return displayResponse(browseBody(3), 200), nil
			})
			result, err := coordinator.RefreshDisplayCatalog(ctx)
			if outcome == "browse" {
				if err != nil || result.Browse == nil || result.State != StateApplied {
					t.Fatalf("browse: %+v %v", result, err)
				}
			} else if result.Browse != nil {
				t.Fatalf("unaccepted result published browse: %+v", result)
			}
			if outcome == "pending" {
				var api *APIStatusError
				if !errors.As(err, &api) || api.Code != CodeAccessSyncPending {
					t.Fatalf("pending error lost: %v", err)
				}
			}
			if outcome == "credential" && !errors.Is(err, ErrMalformedCatalog) {
				t.Fatalf("credential fallback: %v", err)
			}
			if outcome == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel lost: %v", err)
			}
			if (outcome == "session" || outcome == "subject" || outcome == "canceled") && !result.Dropped {
				t.Fatal("late response not dropped")
			}
			if coordinator.LastGood() != retained || coordinator.AdmissionTokens() != tokens ||
				coordinator.receipt != receipt || store.Value != receipt || coordinator.lastGoodDigest != digest || coordinator.catalogTick != catalogTick ||
				result.Catalog != nil || result.Admission != nil || fake.syncCalls != 0 {
				t.Fatal("display request changed or exposed admission/credential/receipt state")
			}
		})
	}
}

func TestDisplayRunnerPreservesOrdinaryPathAndChecksSelectedRevocation(t *testing.T) {
	fixture, server := newRunnerFixture(t, true)
	runner, _, _ := newRunner(t, fixture, server, nil, nil)
	runner.config.Now = func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) }
	client := runner.config.Coordinator.opts.Client
	upstream := client.HTTP
	display, revoke, failBrowse := false, false, false
	var paths []string
	client.HTTP = displayDoer(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/mobile/v1/me" && revoke {
			binding := "1"
			body := strings.Replace(meBody("8", &binding, "not_started"), `"binding_status":"active"`, `"binding_status":"revoked"`, 1)
			return displayResponse(body, 200), nil
		}
		if req.URL.Path == "/api/mobile/v1/gateways" {
			paths = append(paths, req.URL.RawQuery)
			if req.URL.RawQuery == "view=browse" {
				if failBrowse {
					return nil, errors.New("synthetic browse failure")
				}
				return displayResponse(browseBody(3), 200), nil
			}
		}
		return upstream.Do(req)
	})
	runner.config.DisplayOnly = func(context.Context) bool { return display }
	var seenMe []MeResponse
	verified, browsed := 0, 0
	runner.config.OnMe = func(me MeResponse) { seenMe = append(seenMe, me) }
	runner.config.OnVerified = func(MeResponse, CatalogResponse) { verified++ }
	runner.config.OnBrowse = func(BrowseCatalogResponse) { browsed++ }
	if _, _, err := runner.cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	coordinator := runner.config.Coordinator
	retained, receipt := coordinator.LastGood(), coordinator.receipt
	if retained == nil || receipt == nil || fixture.syncCalls != 1 || verified != 1 || len(paths) != 1 || paths[0] != "" {
		t.Fatal("ordinary credential/sync route changed")
	}
	display = true
	runner.config.SelectedNodeID = func() string { return "gw-synth-0" }
	var decisions []AdmissionDecision
	runner.config.OnAdmission = func(decision AdmissionDecision) { decisions = append(decisions, decision) }
	for i := 0; i < 2; i++ {
		catalog, browse, err := runner.cycle(context.Background())
		if err != nil || browse == nil || catalog != retained {
			t.Fatalf("display cycle: %v", err)
		}
	}
	if len(seenMe) != 3 || len(decisions) != 2 || !decisions[1].Admitted || browsed != 2 || verified != 1 {
		t.Fatalf("fresh/duplicate me and display callbacks: me=%d decisions=%+v browsed=%d verified=%d", len(seenMe), decisions, browsed, verified)
	}
	// Fresh revocation must reach the selected VPN even when fetching the list fails.
	revoke, failBrowse = true, true
	if _, _, err := runner.cycle(context.Background()); err == nil {
		t.Fatal("browse transport error hidden")
	}
	if len(decisions) != 3 || !decisions[2].StopDataPlane || decisions[2].Reason != "BINDING_REVOKED" ||
		seenMe[len(seenMe)-1].BindingStatus != "revoked" {
		t.Fatalf("fresh revocation ignored: %+v", decisions)
	}
	if fixture.syncCalls != 1 || fixture.operationCalls != 0 || coordinator.LastGood() != retained || coordinator.receipt != receipt || browsed != 2 {
		t.Fatal("display branch mutated credentials, started sync/poll, or published a failed list")
	}
	for _, query := range paths[1:] {
		if query != "view=browse" {
			t.Fatalf("display request used ordinary query: %q", query)
		}
	}
}

func TestDisplayRunnerDropsCanceledAndStaleMeWithoutCallbacks(t *testing.T) {
	for _, outcome := range []string{"canceled", "session"} {
		t.Run(outcome, func(t *testing.T) {
			fixture, server := newRunnerFixture(t, true)
			runner, _, _ := newRunner(t, fixture, server, nil, nil)
			runner.config.DisplayOnly = func(context.Context) bool { return true }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := runner.config.Coordinator.opts.Client
			upstream := client.HTTP
			client.HTTP = displayDoer(func(req *http.Request) (*http.Response, error) {
				response, err := upstream.Do(req)
				if req.URL.Path == "/api/mobile/v1/me" {
					if outcome == "canceled" {
						cancel()
					} else {
						runner.config.Coordinator.NewSession()
					}
				}
				return response, err
			})
			calls := 0
			runner.config.OnMe = func(MeResponse) { calls++ }
			runner.config.OnBrowse = func(BrowseCatalogResponse) { calls++ }
			runner.config.OnAdmission = func(AdmissionDecision) { calls++ }
			_, _, err := runner.cycle(ctx)
			if outcome == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel lost: %v", err)
			}
			if calls != 0 || fixture.catalogCalls != 0 || fixture.syncCalls != 0 {
				t.Fatal("stale/canceled me started callbacks or catalog/sync")
			}
		})
	}
}
