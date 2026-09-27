package main

import (
	"bufio"
	"context"
	"strings"
	"testing"
)

// The read-loop end cause must come from the exact branch, never from a blanket flag:
// an explicit ctx cancel while a line is available is not a stdin EOF.
func TestBridgeReadEndCauseBranches(t *testing.T) {
	eofBridge := newManagedBridge(discardWriter{}, "a", func() {})
	eofBridge.read(context.Background(), bufio.NewScanner(strings.NewReader("")))
	if got := eofBridge.RunEndToken(); got != "STDIN_EOF" {
		t.Fatalf("eof token=%s", got)
	}

	scanBridge := newManagedBridge(discardWriter{}, "a", func() {})
	scanner := bufio.NewScanner(strings.NewReader(strings.Repeat("x", 64)))
	scanner.Buffer(make([]byte, 4), 8)
	scanBridge.read(context.Background(), scanner)
	if got := scanBridge.RunEndToken(); got != "STDIN_ERROR" {
		t.Fatalf("scanner error token=%s", got)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledBridge := newManagedBridge(discardWriter{}, "a", func() {})
	canceledBridge.read(canceledCtx, bufio.NewScanner(strings.NewReader("{\"v\":1}\n")))
	if got := canceledBridge.RunEndToken(); got != "CTX_ALREADY_CANCELED" {
		t.Fatalf("canceled ctx token=%s", got)
	}

	canceled := false
	cancelBridge := newManagedBridge(discardWriter{}, "a", func() { canceled = true })
	cancelBridge.read(context.Background(),
		bufio.NewScanner(strings.NewReader("{\"v\":1,\"attempt_id\":\"a\",\"type\":\"cancel\"}\n")))
	if got := cancelBridge.RunEndToken(); got != "EXPLICIT_CANCEL" {
		t.Fatalf("explicit cancel token=%s", got)
	}
	if !canceled {
		t.Fatal("explicit cancel must invoke the cancel func")
	}

	first := newManagedBridge(discardWriter{}, "a", func() {})
	first.noteRunEnd(runEndEOF)
	first.noteRunEnd(runEndScanError)
	if got := first.RunEndToken(); got != "STDIN_EOF" {
		t.Fatalf("first cause overwritten: %s", got)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
