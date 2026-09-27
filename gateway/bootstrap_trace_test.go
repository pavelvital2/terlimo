package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wg-turn-client/internal/wlwire"
)

func captureManagedTrace(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	oldOutput := log.Writer()
	oldFlags := log.Flags()
	oldPrefix := log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})
	return &logs
}

func bootstrapTraceFixture(t *testing.T) ClientTestBootstrap {
	t.Helper()
	t.Setenv("WL_TEST_ENABLED", "1")
	b := ClientTestBootstrap{CredentialID: "fixture", Secret: "synthetic-bootstrap-secret-only", ExpiresAt: time.Now().Add(time.Minute).Unix()}
	old := db
	db = &Database{ClientBootstrap: map[string]ClientTestBootstrap{"fixture": b}}
	t.Cleanup(func() { db = old })
	return b
}

func TestBootstrapTraceFramedResponseAndInvalidJSON(t *testing.T) {
	for _, test := range []struct {
		name     string
		response []byte
		wantBody bool
		stage    string
		shape    string
	}{
		{name: "framed_response", response: []byte(`{"v":1,"status":"ok"}`), wantBody: true, stage: "stage=bootstrap_response_write class=none", shape: "response_shape=valid_json"},
		{name: "empty", response: nil, stage: "stage=bootstrap_backend_response class=invalid_json", shape: "response_shape=empty"},
		{name: "invalid_json", response: []byte("not-json"), stage: "stage=bootstrap_backend_response class=invalid_json", shape: "response_shape=invalid_json"},
		{name: "oversize", response: bytes.Repeat([]byte("x"), wlwire.MaxBody+1), stage: "stage=bootstrap_backend_response class=oversize", shape: "response_shape=oversize"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := captureManagedTrace(t)
			bootstrap := bootstrapTraceFixture(t)
			path := filepath.Join(t.TempDir(), "backend.sock")
			t.Setenv("WL_TEST_BACKEND_SOCKET", path)
			backend, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			go func() {
				conn, acceptErr := backend.Accept()
				if acceptErr != nil {
					return
				}
				defer conn.Close()
				var envelope map[string]any
				_ = json.NewDecoder(conn).Decode(&envelope)
				_, _ = conn.Write(test.response)
			}()

			server, client := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				clientTestBootstrapServe(context.Background(), server, bootstrap)
			}()
			var id wlwire.ID
			id[0] = 1
			frames, _ := wlwire.Frames(id, false, []byte(`{"v":1,"op":"challenge"}`))
			for _, frame := range frames {
				if _, err = client.Write(frame); err != nil {
					t.Fatal(err)
				}
			}
			if test.wantBody {
				_ = client.SetReadDeadline(time.Now().Add(time.Second))
				buf := make([]byte, 2048)
				n, readErr := client.Read(buf)
				if readErr != nil {
					t.Fatal(readErr)
				}
				var assembler wlwire.Assembler
				_, body, assembleErr := assembler.Add(buf[:n], time.Now())
				if assembleErr != nil || !bytes.Equal(body, test.response) {
					t.Fatalf("framed response body=%q err=%v", body, assembleErr)
				}
			}
			_ = client.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("bootstrap serve did not stop")
			}
			if !strings.Contains(logs.String(), test.stage) {
				t.Fatalf("missing trace %q in %q", test.stage, logs.String())
			}
			if !strings.Contains(logs.String(), test.shape) {
				t.Fatalf("missing shape %q in %q", test.shape, logs.String())
			}
			if strings.Contains(logs.String(), "conn=") || strings.Contains(logs.String(), bootstrap.Secret) || strings.Contains(logs.String(), `"op":"challenge"`) {
				t.Fatalf("trace exposed forbidden context: %q", logs.String())
			}
		})
	}
}

func TestBootstrapTraceRequestReadEOF(t *testing.T) {
	logs := captureManagedTrace(t)
	bootstrap := bootstrapTraceFixture(t)
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		clientTestBootstrapServe(context.Background(), server, bootstrap)
	}()
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bootstrap serve did not stop after EOF")
	}
	if !strings.Contains(logs.String(), "stage=bootstrap_request_read class=eof") {
		t.Fatalf("missing EOF classification in %q", logs.String())
	}
}
