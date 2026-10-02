package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"wg-turn-client/servicechannel"
	"wg-turn-client/wlwire"
)

// Exercise the actual start -> Runner -> MobileSession -> service Doer/Channel ->
// product bridge path. A complete wire reply is an AUTH failure, then the retry
// establishes again under the SAME cycle. Only the physical establishment is fake.
func TestCatalogAuthServiceFailureRetainsProgressCycleAcrossReconnect(t *testing.T) {
	fixture := newWiringFixture(t)
	seed, err := json.Marshal(servicechannel.Seed{Version: 1, Revision: "1", Environment: "test",
		PeerIP: "192.0.2.7", DTLSPort: 56000, DTLSSPKISHA256: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		ServiceClassifier: "synthetic-public-classifier", VKHashes: []string{"synthetic-hash"}})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bridge := newManagedBridge(&output, "attempt", cancel)
	start := managedStart{V: 1, Type: "start", AttemptID: "attempt", CatalogCycle: cycleA,
		MobileBaseURL: "https://mobile.invalid", MobileEnvironment: "test", ServiceSeed: string(seed)}
	controller := &managedController{bridge: bridge, start: start}
	mobile, err := newManagedMobile(start, fixture.spkiDER,
		func(context.Context, []byte) ([]byte, error) {
			return nil, errors.New("signer must not run after failed challenge")
		}, bridge, controller)
	if err != nil {
		t.Fatal(err)
	}
	doer, ok := mobile.client.HTTP.(*servicechannel.Doer)
	if !ok {
		t.Fatal("production wiring did not select the service channel")
	}
	defer doer.Close()
	var wireStages []string
	doer.Observe = func(stage string) { wireStages = append(wireStages, stage) }
	dials := 0
	serverDone := make(chan error, 1)
	doer.Channel.Establishment = servicechannel.EstablishFunc(func(establishCtx context.Context, _ servicechannel.Seed) (net.Conn, func(), error) {
		dials++
		if catalogCycleFromContext(establishCtx) != cycleA || servicechannel.RequestClass(establishCtx) != "AUTH" {
			t.Error("AUTH establishment lost its operation context")
		}
		if dials == 2 {
			// Stop at the exact second ESTABLISH boundary after its product event.
			cancel()
			return nil, nil, context.Canceled
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var assembler wlwire.ServiceAssembler
			packet := make([]byte, wlwire.ServiceFragment+32)
			for {
				n, readErr := server.Read(packet)
				if readErr != nil {
					serverDone <- readErr
					return
				}
				id, body, frameErr := assembler.Add(packet[:n], time.Now())
				if frameErr != nil {
					serverDone <- frameErr
					return
				}
				if body == nil {
					continue
				}
				var request map[string]any
				if err := json.Unmarshal(body, &request); err != nil {
					serverDone <- err
					return
				}
				if request["path"] != "/api/mobile/v1/auth/challenge" {
					serverDone <- errors.New("unexpected request after failed challenge")
					return
				}
				reply, _ := json.Marshal(map[string]any{"v": 1, "request_id": request["request_id"],
					"error": map[string]any{"code": "SERVICE_UNAVAILABLE", "retryable": true}})
				frames, frameErr := wlwire.ServiceFrames(id, true, reply)
				if frameErr != nil {
					serverDone <- frameErr
					return
				}
				for _, frame := range frames {
					if _, err := server.Write(frame); err != nil {
						serverDone <- err
						return
					}
				}
				serverDone <- nil
				return
			}
		}()
		return client, func() { _ = client.Close(); _ = server.Close() }, nil
	})
	if err := mobile.runner.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("runner terminal: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if dials != 2 {
		t.Fatalf("dials=%d, want failed AUTH then real reconnect", dials)
	}
	sawServiceFailure := false
	for _, stage := range wireStages {
		if stage == "SERVICE_ERROR_SERVICE_UNAVAILABLE" {
			sawServiceFailure = true
		}
	}
	if !sawServiceFailure {
		t.Fatalf("wire reply was not classified as the actual AUTH failure: %v", wireStages)
	}
	var stages []string
	decoder := json.NewDecoder(&output)
	for {
		var message bridgeMessage
		if err := decoder.Decode(&message); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if message.string("type") != "catalog_stage" || message.string("catalog_cycle") != cycleA || message.string("attempt_id") != "attempt" {
			t.Fatalf("unexpected or uncorrelated event: %v", message)
		}
		stages = append(stages, message.string("stage"))
	}
	if want := []string{"connecting_server", "checking_device", "connecting_server"}; !reflect.DeepEqual(stages, want) {
		t.Fatalf("actual product stages=%v want=%v", stages, want)
	}
}
