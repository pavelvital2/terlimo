package main

import (
	"context"
	"errors"
	"testing"
)

func TestRuntimeExitStagesFixedEnums(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		token   string
		cause   error
		outcome string
		source  string
	}{
		{"controller return", nil, "NONE", nil, "OK", "CONTROLLER_RETURN"},
		{"stdin eof", nil, "STDIN_EOF", context.Canceled, "OK", "STDIN_EOF"},
		{"scanner error", context.Canceled, "STDIN_ERROR", context.Canceled, "CANCELED", "STDIN_ERROR"},
		{"ctx already canceled", context.Canceled, "CTX_ALREADY_CANCELED", context.Canceled, "CANCELED", "CTX_ALREADY_CANCELED"},
		{"sigterm wins over token", context.Canceled, "STDIN_EOF", errRunSignalTerm, "CANCELED", "SIGNAL_TERM"},
		{"sigint", context.Canceled, "NONE", errRunSignalInt, "CANCELED", "SIGNAL_INT"},
		{"other cancel", context.Canceled, "NONE", context.Canceled, "CANCELED", "CANCELED_OTHER"},
		{"real error", errors.New("X"), "NONE", nil, "ERROR", "UNKNOWN"},
	}
	for _, c := range cases {
		outcome, source := runtimeExitStages(c.err, c.token, c.cause)
		if outcome != c.outcome || source != c.source {
			t.Fatalf("%s: got %s/%s want %s/%s", c.name, outcome, source, c.outcome, c.source)
		}
	}
}
