package accountaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The accepted contract fixture (contracts-3e12f6f9) is used byte-for-byte.
const announcementsFixtureSHA = "afc26e69aa891845781dd2c6e455431e8ac057572caf0d4a32ee84de415971e2"

func announcementsFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/announcements/announcements_response.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != announcementsFixtureSHA {
		t.Fatalf("announcements fixture drifted: %s", hex.EncodeToString(sum[:]))
	}
	return raw
}

const announcementsReadMarkerJSON = `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-19T15:20:00Z","schema_version":"1.0","status":"ok","announcement_id":"an-1","read":true,"read_at":"2026-09-19T15:21:00Z"}`

func TestAnnouncementsFixtureStrictDecode(t *testing.T) {
	response, err := DecodeAnnouncementsStrict(announcementsFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if response.SchemaVersion != "1.0" || response.Status != "ok" {
		t.Fatalf("bad envelope: %+v", response)
	}
	if response.RequestID != "0123456789abcdef0123456789abcdef" || response.ServerTime != "2026-09-19T15:20:00Z" {
		t.Fatalf("bad server fields: %+v", response)
	}
	if response.UnreadCount != 1 || len(response.Announcements) != 1 {
		t.Fatalf("bad counts: %+v", response)
	}
	announcement := response.Announcements[0]
	if announcement.AnnouncementID != "an-1" || announcement.Revision != "3" || !announcement.Unread {
		t.Fatalf("bad announcement: %+v", announcement)
	}
	if announcement.Action.Type != AnnouncementActionRefreshCatalog {
		t.Fatalf("bad action type: %q", announcement.Action.Type)
	}
	if announcement.Action.Label == nil || *announcement.Action.Label != "Обновить" {
		t.Fatalf("bad action label: %v", announcement.Action.Label)
	}
	if announcement.ValidUntil != nil {
		t.Fatalf("valid_until must stay null, got %v", *announcement.ValidUntil)
	}
	if announcement.Title != "Обновление" || announcement.Text != "Обновите каталог." {
		t.Fatalf("bad text: %+v", announcement)
	}
}

func TestAnnouncementStrictRejections(t *testing.T) {
	base := `{"request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-19T15:20:00Z","schema_version":"1.0","status":"ok","announcements":[{"announcement_id":"an-1","title":"t","text":"x","action":{"type":"none"},"unread":true,"revision":"1","valid_until":null}],"unread_count":1}`
	replace := func(old, new string) string {
		if !strings.Contains(base, old) {
			t.Fatalf("test base missing %q", old)
		}
		return strings.Replace(base, old, new, 1)
	}
	cases := map[string]string{
		"unknown top field":        strings.Replace(base, `"unread_count":1`, `"unread_count":1,"extra":true`, 1),
		"missing top field":        strings.Replace(base, `,"unread_count":1`, "", 1),
		"schema_version":           replace(`"1.0"`, `"2.0"`),
		"status error":             replace(`"status":"ok"`, `"status":"error"`),
		"request_id":               replace(`0123456789abcdef0123456789abcdef`, `not-hex`),
		"server_time":              replace(`2026-09-19T15:20:00Z`, `not-a-time`),
		"announcements null":       strings.Replace(base, `[{"announcement_id":"an-1","title":"t","text":"x","action":{"type":"none"},"unread":true,"revision":"1","valid_until":null}]`, `null`, 1),
		"negative unread_count":    replace(`"unread_count":1`, `"unread_count":-1`),
		"announcement unknown key": replace(`"unread":true`, `"unread":true,"extra":1`),
		"announcement missing key": replace(`,"unread":true`, ""),
		"bad action enum":          replace(`"type":"none"`, `"type":"explode"`),
		"bad revision":             replace(`"revision":"1"`, `"revision":"01"`),
		"bad unread type":          replace(`"unread":true`, `"unread":"true"`),
		"valid_until wrong type":   replace(`"valid_until":null`, `"valid_until":42`),
		"action label wrong type":  replace(`"type":"none"`, `"type":"none","label":7`),
		"title too long":           replace(`"title":"t"`, fmt.Sprintf(`"title":%q`, strings.Repeat("t", 129))),
		"title null":               replace(`"title":"t"`, `"title":null`),
		"text null":                replace(`"text":"x"`, `"text":null`),
		"unread null":              replace(`"unread":true`, `"unread":null`),
		"revision null":            replace(`"revision":"1"`, `"revision":null`),
		"announcement_id null":     replace(`"announcement_id":"an-1"`, `"announcement_id":null`),
		"unread_count null":        replace(`"unread_count":1`, `"unread_count":null`),
		"action label null":        replace(`"type":"none"`, `"type":"none","label":null`),
	}
	for name, raw := range cases {
		if _, err := DecodeAnnouncementsStrict([]byte(raw)); err == nil {
			t.Fatalf("%s: accepted malformed body", name)
		}
	}
}

func TestReadMarkerStrictDecodeAndRejections(t *testing.T) {
	marker, err := DecodeReadMarkerStrict([]byte(announcementsReadMarkerJSON))
	if err != nil {
		t.Fatal(err)
	}
	if marker.AnnouncementID != "an-1" || !marker.Read || marker.ReadAt != "2026-09-19T15:21:00Z" {
		t.Fatalf("bad marker: %+v", marker)
	}
	bad := []string{
		strings.Replace(announcementsReadMarkerJSON, `"read":true`, `"read":"yes"`, 1),
		strings.Replace(announcementsReadMarkerJSON, `,"read_at":"2026-09-19T15:21:00Z"`, "", 1),
		strings.Replace(announcementsReadMarkerJSON, `"read_at":"2026-09-19T15:21:00Z"`, `"read_at":"nope"`, 1),
		strings.Replace(announcementsReadMarkerJSON, `"status":"ok"`, `"status":"error"`, 1),
		strings.Replace(announcementsReadMarkerJSON, `"announcement_id":"an-1"`, `"announcement_id":""`, 1),
		strings.Replace(announcementsReadMarkerJSON, `"read":true`, `"read":null`, 1),
		strings.Replace(announcementsReadMarkerJSON, `"announcement_id":"an-1"`, `"announcement_id":null`, 1),
		strings.Replace(announcementsReadMarkerJSON, `"read_at":"2026-09-19T15:21:00Z"`, `"read_at":null`, 1),
	}
	for i, raw := range bad {
		if _, err := DecodeReadMarkerStrict([]byte(raw)); err == nil {
			t.Fatalf("case %d: accepted malformed read marker", i)
		}
	}
}

// TestAnnouncementsClientForwardingAndIdempotency proves GET /announcements and the
// idempotent POST /announcements/{id}/read forward the host-owned key verbatim on every
// attempt, and that invalid host input never reaches the network.
func TestAnnouncementsClientForwardingAndIdempotency(t *testing.T) {
	fixture := announcementsFixture(t)
	var keys []string
	var listCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "" {
			t.Errorf("missing bearer on %s", request.URL.Path)
		}
		switch request.URL.Path {
		case "/api/mobile/v1/announcements":
			listCalls++
			_, _ = writer.Write(fixture)
		case "/api/mobile/v1/announcements/an-1/read":
			keys = append(keys, request.Header.Get("Idempotency-Key"))
			_, _ = writer.Write([]byte(announcementsReadMarkerJSON))
		default:
			t.Errorf("unexpected path %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: StaticToken("bearer")}

	list, apiError, err := client.ListAnnouncements(context.Background())
	if err != nil || apiError != nil {
		t.Fatalf("list failed: api=%v err=%v", apiError, err)
	}
	if list.UnreadCount != 1 || listCalls != 1 {
		t.Fatalf("bad list result: %+v calls=%d", list, listCalls)
	}

	const hostKey = "host-key-0000000000000000000000000042"
	for attempt := 0; attempt < 2; attempt++ {
		marker, apiError, err := client.MarkAnnouncementRead(context.Background(), "an-1", hostKey)
		if err != nil || apiError != nil {
			t.Fatalf("mark failed: api=%v err=%v", apiError, err)
		}
		if !marker.Read {
			t.Fatalf("marker not read: %+v", marker)
		}
	}
	if len(keys) != 2 || keys[0] != hostKey || keys[1] != hostKey {
		t.Fatalf("idempotency key not forwarded verbatim: %v", keys)
	}
}

func TestAnnouncementsClientRejectsInvalidInputWithoutRequest(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		called = true
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: StaticToken("bearer")}

	if _, _, err := client.MarkAnnouncementRead(context.Background(), "..", "host-key-0000000000000000000000000042"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("traversal id not rejected locally: %v", err)
	}
	if _, _, err := client.MarkAnnouncementRead(context.Background(), "an-1", "short"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("short key not rejected locally: %v", err)
	}
	if called {
		t.Fatal("invalid host input reached the network")
	}
}

// TestAnnouncementReadPathIDBoundary pins the single unreserved-segment grammar shared
// with the service-channel wire allowlist: ids that the wire gate would reject (colon,
// non-ASCII, percent, slash, dot-dot, control) are refused locally before any request, so
// there is no late unexpected reject. TEST fixture ids ("an-1") are unreserved and pass.
func TestAnnouncementReadPathIDBoundary(t *testing.T) {
	valid := []string{"an-1", "A.b~c-1", strings.Repeat("a", 128)}
	for _, id := range valid {
		if !ValidAnnouncementPathID(id) {
			t.Fatalf("valid read-marker id rejected: %q", id)
		}
	}
	invalid := []string{"", ".", "..", "a..b", "an:1", "ан-1", "a%2Fb", "a/b", "a b",
		"a\b", "a?b", "a#b", strings.Repeat("a", 129), "a\rb"}
	for _, id := range invalid {
		if ValidAnnouncementPathID(id) {
			t.Fatalf("read-marker id accepted although the wire gate would reject it: %q", id)
		}
	}
}

func TestAnnouncementsClientAPIErrorEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write([]byte(`{"status":"error","code":"IDEMPOTENCY_CONFLICT","request_id":"0123456789abcdef0123456789abcdef","server_time":"2026-09-19T15:20:00Z","schema_version":"1.0","retryable":false}`))
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL + "/api/mobile/v1", HTTP: server.Client(), Tokens: StaticToken("bearer")}
	_, apiError, err := client.MarkAnnouncementRead(context.Background(), "an-1", "host-key-0000000000000000000000000042")
	if err != nil {
		t.Fatal(err)
	}
	if apiError == nil || apiError.Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("bad api error: %+v", apiError)
	}
}
