package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestCatalogDisplayProjectionNoSecrets(t *testing.T) {
	cat := runnerCatalog(time.Now().UTC().Truncate(time.Second))
	var out bytes.Buffer
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &managedController{bridge: newManagedBridge(&out, "synthetic", cancel)}
	if err := c.store.Apply(cat, "sub", "reg", time.Now()); err != nil {
		t.Fatal(err)
	}
	c.publishCatalog()
	var message map[string]any
	if err := json.NewDecoder(&out).Decode(&message); err != nil {
		t.Fatal(err)
	}
	expected := []string{"v", "attempt_id", "type", "nodes", "selected_node_id", "slots_used", "slots_limit", "subscription_status", "subscription_expires_at", "catalog_expires_at", "issued_at", "revision"}
	if len(message) != len(expected) {
		t.Fatal("unexpected display field")
	}
	for _, key := range expected {
		if _, ok := message[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	if message["subscription_expires_at"] != cat.SubscriptionExpiresAt || message["catalog_expires_at"] != cat.CatalogExpiresAt {
		t.Fatal("expiry projection changed")
	}
	if message["revision"] != cat.Revision {
		t.Fatal("revision projection changed")
	}
	node := message["nodes"].([]any)[0].(map[string]any)
	if len(node) != 3 || node["node_id"] != "test" || node["name"] != "Test" || node["country_code"] != "RU" {
		t.Fatal("node display leaked fields")
	}
}

func TestCatalogV2DisplayUsesExplicitInternalUnlimitedProjection(t *testing.T) {
	cat := runnerCatalog(time.Now().UTC().Truncate(time.Second))
	cat.V = 2
	cat.SubscriptionExpiresAt = ""
	cat.SubscriptionUnlimited = true
	for i := range cat.Nodes {
		cat.Nodes[i].Access.ExpiresAt = ""
		cat.Nodes[i].Access.Unlimited = true
	}
	var out bytes.Buffer
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &managedController{bridge: newManagedBridge(&out, "synthetic", cancel)}
	if err := c.store.Apply(cat, "sub", "reg", time.Now()); err != nil {
		t.Fatal(err)
	}
	c.publishCatalog()
	var message map[string]any
	if err := json.NewDecoder(&out).Decode(&message); err != nil {
		t.Fatal(err)
	}
	if message["catalog_version"] != float64(2) || message["subscription_unlimited"] != true || message["subscription_expires_at"] != nil {
		t.Fatal("v2 unlimited display projection is not explicit")
	}
}
