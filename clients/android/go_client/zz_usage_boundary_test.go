package main

import (
	"errors"
	"strings"
	"testing"

	"wg-turn-client/servicechannel"
)

func TestUsageCauseTokenDistinguishesLocalServiceAndTransport(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"seed", servicechannel.ErrSeedMissing, "FAIL_LOCAL_SEED_MISSING"},
		{"origin", servicechannel.ErrOriginRejected, "FAIL_LOCAL_ORIGIN_REJECTED"},
		{"path", servicechannel.ErrPathRejected, "FAIL_LOCAL_PATH_REJECTED"},
		{"request", servicechannel.ErrRequestRejected, "FAIL_LOCAL_REQUEST_REJECTED"},
		{"response", servicechannel.ErrResponseRejected, "FAIL_SERVICE_RESPONSE_REJECTED"},
		{"relay denial", &servicechannel.ServiceError{Code: "SERVICE_PATH_DENIED"}, "FAIL_SERVICE_PATH_DENIED"},
		{"service unavailable", &servicechannel.ServiceError{Code: "SERVICE_UNAVAILABLE"}, "FAIL_SERVICE_UNAVAILABLE"},
		{"unknown service code", &servicechannel.ServiceError{Code: "SOMETHING_NEW"}, "FAIL_SERVICE_ERROR"},
		{"plain transport", errors.New("read tcp: raw secret detail"), "FAIL_TRANSPORT"},
	}
	for _, tc := range cases {
		if got := usageCauseToken(tc.err); got != tc.want {
			t.Fatalf("%s: want %q got %q", tc.name, tc.want, got)
		}
	}
}

// The detailed cause marker is internal; the frozen public usage_result code stays the
// host-accepted set (TRANSPORT here), never the FAIL_* diagnostic token.
func TestUsageCauseTokenNeverBecomesThePublicCode(t *testing.T) {
	for _, err := range []error{
		servicechannel.ErrSeedMissing,
		&servicechannel.ServiceError{Code: "SERVICE_PATH_DENIED"},
		errors.New("raw secret detail"),
	} {
		if got := paymentTransportCode(err, false); got != "TRANSPORT" {
			t.Fatalf("public code for %v = %q, want TRANSPORT", err, got)
		}
		if strings.HasPrefix(usageCauseToken(err), "FAIL_") == false {
			t.Fatalf("cause token not bounded FAIL_*: %q", usageCauseToken(err))
		}
	}
}

func TestUsageStagesAreBoundedAndRawFree(t *testing.T) {
	var buf strings.Builder
	previous := diagStageDiagnostics
	diagStageDiagnostics = &buf
	defer func() { diagStageDiagnostics = previous }()

	emitUsageStage(usageResponseStage(200))
	emitUsageStage(usageResponseStage(403))
	emitUsageStage(usageResponseStage(503))
	emitUsageStage(usageResponseStage(100))
	emitUsageStage(usageCauseToken(servicechannel.ErrPathRejected))
	emitUsageStage(usageCauseToken(&servicechannel.ServiceError{Code: "SERVICE_PATH_DENIED"}))
	emitUsageStage("raw secret reason")
	want := "usagestage: RESPONSE_2XX\nusagestage: RESPONSE_4XX\nusagestage: RESPONSE_5XX\n" +
		"usagestage: RESPONSE_OTHER\nusagestage: FAIL_LOCAL_PATH_REJECTED\nusagestage: FAIL_SERVICE_PATH_DENIED\n"
	if buf.String() != want {
		t.Fatalf("unexpected usagestage output:\n got %q\nwant %q", buf.String(), want)
	}
	if strings.Contains(buf.String(), "secret") {
		t.Fatalf("raw text leaked: %q", buf.String())
	}
}

func TestRuntimeCancelProducerMarkerIsBounded(t *testing.T) {
	var buf strings.Builder
	previous := diagStageDiagnostics
	diagStageDiagnostics = &buf
	defer func() { diagStageDiagnostics = previous }()

	emitRuntimeCancel(vpnCancelHostStop)
	emitRuntimeCancel(vpnCancelConnectBudget)
	emitRuntimeCancel(vpnCancelWorkerTerminal)
	emitRuntimeCancel(vpnCancelWorkerGate)
	emitRuntimeCancel(vpnCancelWorkerCreds)
	emitRuntimeCancel("raw secret reason")
	want := "vpnstage: RUNTIME_CANCEL HOST_STOP\nvpnstage: RUNTIME_CANCEL CONNECT_BUDGET\n" +
		"vpnstage: RUNTIME_CANCEL WORKER_TERMINAL\nvpnstage: RUNTIME_CANCEL WORKER_GATE\n" +
		"vpnstage: RUNTIME_CANCEL WORKER_CREDS\nvpnstage: RUNTIME_CANCEL UNKNOWN\n"
	if buf.String() != want {
		t.Fatalf("unexpected runtime cancel output:\n got %q\nwant %q", buf.String(), want)
	}
}
