package main

import (
	"regexp"
	"strings"
	"testing"

	"wg-turn-client/servicechannel"
)

// Fixed-grammar check for the secret-free svctrace line: bounded counters, fixed tokens
// and fixed error classes only; no payload-bearing field exists.
func TestFormatServiceTraceFixedGrammar(t *testing.T) {
	line := formatServiceTrace(servicechannel.TraceEvent{Session: 2, Exchange: 17, Class: "GATEWAYS",
		Event: "READ_FAIL", Reused: true, Requests: 3, IdleMS: 1250, ElapsedMS: 88, ErrClass: "EOF", Port: 51409})
	want := "svctrace: gen=2 class=GATEWAYS event=READ_FAIL reused=1 port=51409 xid=17 elapsed_ms=88 req=3 idle_ms=1250 err=EOF"
	if line != want {
		t.Fatalf("line=%q\nwant %q", line, want)
	}
	pattern := regexp.MustCompile(`^svctrace: gen=[0-9]{1,10} class=[A-Z_]{1,16} event=[A-Z_]{1,16} reused=[01] ` +
		`port=[0-9]{1,5} xid=[0-9]{1,10} elapsed_ms=[0-9]{1,7}( req=[0-9]{1,2})?( idle_ms=[0-9]{1,9})?( err=[A-Z]{1,8})?$`)
	cases := []servicechannel.TraceEvent{
		{Session: 1, Exchange: 1, Class: "AUTH", Event: "ESTABLISH_OK", ElapsedMS: 0, Port: 0},
		{Session: 1, Exchange: 2, Class: "ME", Event: "WRITE_BEGIN", Reused: true, Requests: 2, IdleMS: 5, Port: 41000},
		{Session: 1, Exchange: 3, Class: "ACCESS_SYNC", Event: "EXCHANGE_END", Reused: true, Requests: 8, IdleMS: 8001, ElapsedMS: 15000, ErrClass: "TIMEOUT", Port: 65535},
		{Session: 99, Exchange: 9999999999, Class: "OTHER", Event: "REUSE_STALE_IDLE", Reused: true, Requests: 8, IdleMS: 999999999, Port: 1},
	}
	for _, ev := range cases {
		if got := formatServiceTrace(ev); !pattern.MatchString(got) {
			t.Fatalf("line %q does not match the fixed grammar", got)
		}
	}
	none := formatServiceTrace(servicechannel.TraceEvent{Session: 1, Exchange: 1, Class: "ME",
		Event: "READ_OK", ElapsedMS: 5, ErrClass: "NONE", Port: 1})
	if strings.Contains(none, "err=") {
		t.Fatalf("NONE must never be emitted: %q", none)
	}
	// A raw error text can never reach the line: only the fixed class field exists.
	raw := formatServiceTrace(servicechannel.TraceEvent{Session: 1, Exchange: 1, Class: "ME",
		Event: "READ_FAIL", ElapsedMS: 5, ErrClass: "OTHER", Port: 1})
	if strings.Contains(raw, "dial") || strings.Contains(raw, "http") || strings.Contains(raw, "token") {
		t.Fatalf("line must not contain raw text: %q", raw)
	}
}
