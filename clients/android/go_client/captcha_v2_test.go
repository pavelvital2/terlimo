package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseCaptchaV2PageUsesActualScriptMetadata(t *testing.T) {
	html := `
		<html>
			<head>
				<script src='//static.vk.ru/vkid/1.1.1359/not_robot_captcha.js?hash=abc'></script>
				<script>
					window.init = {"data":{"show_captcha_type":"slider","captcha_settings":[{"type":"slider","settings":"{\"nested\":true}"}]}};
					const powInput = "abc";
					const difficulty = 3;
				</script>
			</head>
		</html>`

	page, err := parseCaptchaV2Page(html)
	if err != nil {
		t.Fatalf("parseCaptchaV2Page returned error: %v", err)
	}
	if page.ScriptURL != "https://static.vk.ru/vkid/1.1.1359/not_robot_captcha.js?hash=abc" {
		t.Fatalf("unexpected script URL: %s", page.ScriptURL)
	}
	if page.ScriptVersion != "1.1.1359" {
		t.Fatalf("unexpected script version: %s", page.ScriptVersion)
	}
	if page.Domain != "vk.ru" {
		t.Fatalf("unexpected domain: %s", page.Domain)
	}
	if page.PowInput != "abc" || page.PowDifficulty != 3 {
		t.Fatalf("unexpected pow settings: input=%q difficulty=%d", page.PowInput, page.PowDifficulty)
	}
	if page.Init == nil || page.Init.Data.ShowCaptchaType != "slider" {
		t.Fatalf("unexpected init payload: %#v", page.Init)
	}
}

func TestExtractCaptchaV2WindowInitWithNestedObject(t *testing.T) {
	html := `
		<script>
			window.init = {"data":{"captcha_settings":[{"type":"slider","settings":"{\"a\":{\"b\":1}}"}]}};
			window.after = true;
		</script>`

	got, err := extractCaptchaV2WindowInit(html)
	if err != nil {
		t.Fatalf("extractCaptchaV2WindowInit returned error: %v", err)
	}
	want := `{"data":{"captcha_settings":[{"type":"slider","settings":"{\"a\":{\"b\":1}}"}]}}`
	if got != want {
		t.Fatalf("unexpected init json:\nwant %s\ngot  %s", want, got)
	}
}

func TestInferCaptchaV2DomainPrefersChallengeDomain(t *testing.T) {
	got := inferCaptchaV2Domain(
		"https://id.vk.ru/captcha?session_token=abc&domain=vk.com",
		"https://static.vk.ru/vkid/1.1.1359/not_robot_captcha.js",
	)
	if got != "vk.com" {
		t.Fatalf("unexpected domain: %s", got)
	}
}

func TestInferCaptchaV2DomainFallsBackToScriptURL(t *testing.T) {
	got := inferCaptchaV2Domain("", "https://static.vk.com/vkid/1.1.1367/not_robot_captcha.js")
	if got != "vk.com" {
		t.Fatalf("unexpected domain: %s", got)
	}
}

func TestEncodeCaptchaPoWV2(t *testing.T) {
	got := encodeCaptchaPoWV2("00abc", 42)
	want := "v2.eyJoYXNoIjoiMDBhYmMiLCJub25jZSI6NDJ9"
	if got != want {
		t.Fatalf("unexpected PoW token: %s", got)
	}
}

func TestParseVkCaptchaErrorReadsAdFP(t *testing.T) {
	got := parseVkCaptchaError(map[string]interface{}{
		"error_code":   float64(14),
		"redirect_uri": "https://id.vk.com/captcha?session_token=session&adFp=fingerprint&domain=vk.com",
		"remixstlid":   "tmp-user",
	})
	if got.SessionToken != "session" || got.AdFP != "fingerprint" || got.RemixStlid != "tmp-user" {
		t.Fatalf("unexpected captcha challenge: %#v", got)
	}
}

func TestCaptchaV2SessionAdFPUsesChallengeValue(t *testing.T) {
	got, err := captchaV2SessionAdFP(" challenge-fp ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "challenge-fp" {
		t.Fatalf("unexpected adFp: %q", got)
	}
}

func TestCaptchaV2SessionAdFPGeneratesURLSafeValue(t *testing.T) {
	got, err := captchaV2SessionAdFP("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 22 {
		t.Fatalf("unexpected generated adFp length: %d (%q)", len(got), got)
	}
	for _, ch := range got {
		if !(ch >= 'A' && ch <= 'Z') && !(ch >= 'a' && ch <= 'z') && !(ch >= '0' && ch <= '9') && ch != '-' && ch != '_' {
			t.Fatalf("generated adFp is not base64url-safe: %q", got)
		}
	}
}

func TestParseCaptchaV2SettingsAcceptsMapPayload(t *testing.T) {
	got, err := parseCaptchaV2Settings(map[string]any{
		"response": map[string]any{
			"show_captcha_type": "slider",
			"captcha_settings": map[string]any{
				"slider": map[string]any{"variant": "fresh"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ShowType != "slider" || got.ByType["slider"] != `{"variant":"fresh"}` {
		t.Fatalf("unexpected settings: %#v", got)
	}
}

func TestParseCaptchaV2SettingsAcceptsStringPayload(t *testing.T) {
	got, err := parseCaptchaV2Settings(map[string]any{
		"response": map[string]any{
			"captcha_settings": `[{"type":"slider","settings":"live"}]`,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ByType["slider"] != "live" {
		t.Fatalf("unexpected settings: %#v", got)
	}
}

func TestCaptchaSolveStageUsesFreshChallengePerFallback(t *testing.T) {
	want := []string{"Auto WebView", "Auto WebView", "Go v2", "Manual WebView"}
	for attempt, expected := range want {
		got, ok := captchaSolveStage(attempt + 1)
		if !ok || got != expected {
			t.Fatalf("attempt %d: got %q ok=%v, want %q", attempt+1, got, ok, expected)
		}
	}
	if got := captchaSolveStageCount(); got != len(want) {
		t.Fatalf("unexpected stage count: got %d, want %d", got, len(want))
	}
	if _, ok := captchaSolveStage(5); ok {
		t.Fatal("unexpected fifth captcha stage")
	}
}

func TestCaptchaStageResetsAfterSolverSuccess(t *testing.T) {
	for attempt := 1; attempt <= captchaSolveStageCount(); attempt++ {
		if got := captchaStageAfterSolverSuccess(attempt); got != 1 {
			t.Fatalf("success after stage %d should restart at Auto WebView, got %d", attempt, got)
		}
	}
	got, softFailures := captchaNextStageAfterSolverFailure(2, errors.New("hard failure"), 0)
	if got != 3 || softFailures != 0 {
		t.Fatalf("failure after stage 2 should continue at stage 3, got %d", got)
	}
}

func TestCaptchaAutoSoftFailureRetriesFreshAuto(t *testing.T) {
	got, softFailures := captchaNextStageAfterSolverFailure(
		1,
		errors.New("request fresh captcha challenge: Auto WebView: webview captcha failed: error:auto_no_result"),
		0,
	)
	if got != 1 || softFailures != 1 {
		t.Fatalf("soft auto failure should retry fresh Auto WebView, got stage=%d soft=%d", got, softFailures)
	}

	got, softFailures = captchaNextStageAfterSolverFailure(
		1,
		errors.New("request fresh captcha challenge: Auto WebView: webview captcha failed: error:auto_check_not_sent"),
		captchaAutoSoftFailureLimit,
	)
	if got != 2 || softFailures != captchaAutoSoftFailureLimit {
		t.Fatalf("soft auto failures after limit should advance, got stage=%d soft=%d", got, softFailures)
	}
}

func TestBuildCaptchaRetryDataWithoutCaptchaSid(t *testing.T) {
	got := buildCaptchaRetryData(
		"join-code",
		"Test+User",
		"anon-token",
		&VkCaptchaError{RemixStlid: "tmp/user"},
		"success/token",
	)
	want := "vk_join_link=https://vk.ru/call/join/join-code&name=Test+User&success_token=success%2Ftoken&access_token=anon-token&remixstlid=tmp%2Fuser"
	if got != want {
		t.Fatalf("unexpected retry data:\nwant %s\ngot  %s", want, got)
	}
}

func TestCaptchaSliderFailureAdvancesPerOfficialMapper(t *testing.T) {
	// Donor v20 keeps the slider/kaleidoscope answer inside the normal stage order: the
	// Android dispatcher opens the manual window for selected mode, while the plain stage
	// order advances one step at a time.
	got, soft := captchaNextStageAfterSolverFailure(1, errors.New("webview captcha failed: error:slider_detected"), 0)
	if got != 2 || soft != 0 {
		t.Fatalf("slider mapping=(%d,%d), want (2,0)", got, soft)
	}
	got, _ = captchaNextStageAfterSolverFailure(2, errors.New("webview captcha failed: error:slider_detected"), 0)
	if got != 3 {
		t.Fatalf("second slider mapping=%d, want 3", got)
	}
	got, soft = captchaNextStageAfterSolverFailure(3, errors.New("webview captcha failed: error:slider_detected"), 0)
	if got != 4 || soft != 0 {
		t.Fatalf("third slider mapping=(%d,%d), want (4,0)", got, soft)
	}
}

func TestCaptchaRefusedPromptNeedsSafeWaitWithoutNewChallenge(t *testing.T) {
	previousOutput := managedCaptchaOutput
	previousChan := CaptchaResultChan
	defer func() {
		managedCaptchaOutput = previousOutput
		CaptchaResultChan = previousChan
	}()
	CaptchaResultChan = make(chan CaptchaResult, 1)
	managedCaptchaOutput = func(id, mode, redirect, token string) {
		CaptchaResultChan <- CaptchaResult{RequestID: id, Value: "error:cancelled"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Official order: a refusal is an ordinary stage failure. Earlier stages advance to
	// the next challenge (no terminal claim from a single classifier)...
	_, err := solveCaptchaBySelectedMode(ctx, 0, 1,
		&VkCaptchaError{RedirectURI: "https://vk.com/captcha", SessionToken: "s"}, nil, Profile{}, nil)
	if !errors.Is(err, errCaptchaNextChallenge) || !strings.Contains(err.Error(), "error:cancelled") {
		t.Fatalf("stage 1 refusal must advance with its reason, got %v", err)
	}
	next, _, waitRequired := captchaOuterDecision(err, 1, 0)
	if waitRequired || next != 2 {
		t.Fatalf("stage 1 outer decision=(next=%d,wait=%v), want stage 2", next, waitRequired)
	}

	// ...and only the last stage falls through to the standard safe wait.
	_, err = solveCaptchaBySelectedMode(ctx, 0, captchaSolveStageCount(),
		&VkCaptchaError{RedirectURI: "https://vk.com/captcha", SessionToken: "s"}, nil, Profile{}, nil)
	if err == nil || errors.Is(err, errCaptchaNextChallenge) {
		t.Fatalf("last-stage refusal must be a plain failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "error:cancelled") {
		t.Fatalf("refusal reason must reach the outer layers, got %v", err)
	}
	lastAttempt, soft, waitRequired := captchaOuterDecision(err, captchaSolveStageCount(), 0)
	if !waitRequired || lastAttempt != captchaSolveStageCount() || soft != 0 {
		t.Fatalf("last-stage outer decision=(%d,%d,wait=%v), want safe wait", lastAttempt, soft, waitRequired)
	}
	// Worker/group retry layer: the safe wait stays recoverable and keeps its bounded delay.
	if isTerminalGroupCredentialError(errors.New("CAPTCHA_WAIT_REQUIRED")) {
		t.Fatal("safe wait must stay recoverable for another group fetch")
	}
	if got := groupCredentialRetryDelay(errors.New("CAPTCHA_WAIT_REQUIRED")); got != 90*time.Second {
		t.Fatalf("safe wait retry delay = %v, want 90s", got)
	}
}

func TestCaptchaStageFailureDecisionKeepsOfficialOrder(t *testing.T) {
	// Fresh-challenge errors from stages 1-3 advance exactly one stage; the last stage
	// falls through to the safe wait.
	for attempt := 1; attempt < captchaSolveStageCount(); attempt++ {
		err := fmt.Errorf("%w: stage %d", errCaptchaNextChallenge, attempt)
		next, _, waitRequired := captchaOuterDecision(err, attempt, 0)
		if waitRequired || next != attempt+1 {
			t.Fatalf("attempt %d decision=(%d,wait=%v), want next stage", attempt, next, waitRequired)
		}
	}
	next, _, waitRequired := captchaOuterDecision(errors.New("last stage failed"), captchaSolveStageCount(), 0)
	if !waitRequired || next != captchaSolveStageCount() {
		t.Fatalf("last stage decision=(%d,wait=%v), want safe wait", next, waitRequired)
	}
}
