package accountaccess

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var stageLine = regexp.MustCompile(`^acctstage: ([A-Z_]+) ([0-9]{1,7})$`)

func stageTokens(t *testing.T, buf *strings.Builder) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	tokens := make([]string, 0, len(lines))
	previous := -1
	for i, line := range lines {
		match := stageLine.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("line %d not in the fixed format: %q", i, line)
		}
		if len(line) > 96 {
			t.Fatalf("stage line too long: %q", line)
		}
		ms, err := strconv.Atoi(match[2])
		if err != nil {
			t.Fatalf("elapsed not numeric at %d: %q", i, line)
		}
		if ms < previous {
			t.Fatalf("elapsed not monotonic at %d: %d < %d in %q", i, ms, previous, lines)
		}
		previous = ms
		tokens = append(tokens, match[1])
	}
	return tokens
}

// The fixed stage chronology must survive a real Ensure and never carry anything but the
// exact token plus a bounded monotonic elapsed value.
func TestStageMarkersSecretFreeAndOrdered(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.installations[InstallationFingerprint(fixture.spkiDER)] = true
	fixture.linked = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	session := newFixtureSession(t, fixture, server)

	var buf strings.Builder
	previous := stageDiagnostics
	stageDiagnostics = &buf
	defer func() { stageDiagnostics = previous }()

	if err := session.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	want := []string{stageAuthBegin, stageServiceResponseReceived, stageChallengeDecoded,
		stageSignBegin, stageSignEnd, stageSessionPostBegin, stageSessionResponse}
	if got := stageTokens(t, &buf); !equalStrings(got, want) {
		t.Fatalf("stage chronology: got %v want %v", got, want)
	}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		for _, secret := range []string{server.URL, "Bearer", "spki", "nonce", "/api/", "http"} {
			if strings.Contains(line, secret) {
				t.Fatalf("forbidden value %q leaked in %q", secret, line)
			}
		}
	}
}

// A preferred round denied with ACCESS_DENIED runs a second challenge/sign/session round; the
// chronology must keep both rounds and stay monotonic.
func TestStageChronologySurvivesUnlinkedFallbackRound(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.installations[InstallationFingerprint(fixture.spkiDER)] = true
	fixture.linked = false // preferred access:sync is denied, the unlinked fallback succeeds
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	session := newFixtureSession(t, fixture, server)

	var buf strings.Builder
	previous := stageDiagnostics
	stageDiagnostics = &buf
	defer func() { stageDiagnostics = previous }()

	if err := session.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if scopes := session.Scopes(); !containsString(scopes, "management-only") {
		t.Fatalf("fallback scopes wrong: %v", scopes)
	}

	round := []string{stageServiceResponseReceived, stageChallengeDecoded,
		stageSignBegin, stageSignEnd, stageSessionPostBegin, stageSessionResponse}
	want := append([]string{stageAuthBegin}, append(append([]string{}, round...), round...)...)
	got := stageTokens(t, &buf)
	if !equalStrings(got, want) {
		t.Fatalf("fallback chronology: got %v want %v", got, want)
	}
	if len(got) != 13 {
		t.Fatalf("fallback must keep both rounds: %d lines", len(got))
	}
}

// A transport failure on the session POST is an explicit SESSION_POST_END completion, never a
// received SESSION_RESPONSE.
func TestStageTransportFailureIsPostEndNotResponse(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.installations[InstallationFingerprint(fixture.spkiDER)] = true
	fixture.linked = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	base := newFixtureSession(t, fixture, server)
	base.config.HTTP = failingDoer{inner: server.Client(), path: "/api/mobile/v1/auth/session"}

	var buf strings.Builder
	previous := stageDiagnostics
	stageDiagnostics = &buf
	defer func() { stageDiagnostics = previous }()

	_ = base.Ensure(context.Background())
	got := stageTokens(t, &buf)
	want := []string{stageAuthBegin, stageServiceResponseReceived, stageChallengeDecoded,
		stageSignBegin, stageSignEnd, stageSessionPostBegin, stageSessionPostEnd}
	if !equalStrings(got, want) {
		t.Fatalf("transport chronology: got %v want %v", got, want)
	}
}

type failingDoer struct {
	inner Doer
	path  string
}

func (d failingDoer) Do(request *http.Request) (*http.Response, error) {
	if request.URL.Path == d.path {
		return nil, context.DeadlineExceeded
	}
	return d.inner.Do(request)
}

func TestStageUnknownCollapsesAndElapsedClamps(t *testing.T) {
	var buf strings.Builder
	previous := stageDiagnostics
	stageDiagnostics = &buf
	defer func() { stageDiagnostics = previous }()

	emitStage("NOT_A_STAGE", 5*time.Second)
	emitStage(stageSignBegin, -time.Second)
	emitStage(stageSignEnd, 99*time.Hour)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if lines[0] != "acctstage: STAGE_UNKNOWN 5000" {
		t.Fatalf("unknown token: %q", lines[0])
	}
	if lines[1] != "acctstage: SIGN_BEGIN 0" {
		t.Fatalf("negative elapsed: %q", lines[1])
	}
	if lines[2] != "acctstage: SIGN_END 3600000" {
		t.Fatalf("elapsed clamp: %q", lines[2])
	}
}

func equalStrings(got, want []string) bool {
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
