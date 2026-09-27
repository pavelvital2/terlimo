package wlbs

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func unlimitedCatalog() Catalog {
	c := testCatalog()
	c.V = 2
	c.SubscriptionExpiresAt = ""
	c.SubscriptionUnlimited = true
	for i := range c.Nodes {
		c.Nodes[i].Access.ExpiresAt = ""
		c.Nodes[i].Access.Unlimited = true
	}
	return c
}

func TestCatalogV2ExplicitNullExpiryRoundTripAndAdmission(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c := unlimitedCatalog()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"subscription_expires_at":null`) || !strings.Contains(text, `"expires_at":null`) {
		t.Fatal("v2 unlimited expiry was not encoded as explicit null")
	}
	var decoded Catalog
	if err = StrictJSON(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.SubscriptionUnlimited || !decoded.Nodes[0].Access.Unlimited {
		t.Fatal("explicit null unlimited markers were lost")
	}
	if err = decoded.ValidateNode("fixture-only", "registration", "test-1", now); err != nil {
		t.Fatalf("unlimited access rejected: %v catalog=%+v access=%+v", err, decoded, decoded.Nodes[0].Access)
	}
	if !decoded.Nodes[0].Access.ValidAt(now.Add(365 * 24 * time.Hour)) {
		t.Fatal("unlimited access acquired an artificial deadline")
	}
	store := &CatalogStore{}
	if err = store.Apply(&decoded, "fixture-only", "registration", now); err != nil {
		t.Fatal(err)
	}
	persisted, _ := json.Marshal(store.Snapshot())
	if !strings.Contains(string(persisted), `"subscription_expires_at":null`) || !strings.Contains(string(persisted), `"expires_at":null`) {
		t.Fatal("atomic persistence round-trip replaced null expiry")
	}
}

func TestCatalogV2UnlimitedStillRequiresFreshCatalogHorizon(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fresh := unlimitedCatalog()
	if err := fresh.Validate("fixture-only", "registration", now); err != nil {
		t.Fatal("fresh v2 rejected", err)
	}
	expired := fresh
	expired.IssuedAt = now.Add(-16 * time.Minute).Format(time.RFC3339)
	expired.RefreshAfter = now.Add(-8 * time.Minute).Format(time.RFC3339)
	expired.CatalogExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
	if err := expired.Validate("fixture-only", "registration", now); err == nil {
		t.Fatal("expired v2 catalog admitted")
	}
}

func TestCatalogExpiryVersionDiscriminatorFailsClosed(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	finite := testCatalog()
	unlimited := unlimitedCatalog()
	cases := []Catalog{
		func() Catalog { c := finite; c.V = 2; return c }(),
		func() Catalog { c := unlimited; c.V = 1; return c }(),
	}
	for _, c := range cases {
		if err := c.Validate("fixture-only", "registration", now); err == nil {
			t.Fatal("version/expiry mismatch accepted")
		}
	}
}

func TestCatalogV2UnlimitedPreservesRevocationAndGenerationFences(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, mutate := range []func(*Catalog){
		func(c *Catalog) { c.SubscriptionStatus = "revoked" },
		func(c *Catalog) { c.Nodes[0].Access.Generation = "" },
		func(c *Catalog) { c.Nodes[0].Access.LeaseSeq = "-1" },
		func(c *Catalog) { c.Nodes[0].Access.DeviceID = "other" },
		func(c *Catalog) { c.Nodes[0].MaxWorkers = 0 },
	} {
		c := unlimitedCatalog()
		mutate(&c)
		if err := c.Validate("fixture-only", "registration", now); err == nil {
			t.Fatal("v2 bypassed an existing admission fence")
		}
	}
}

func TestCatalogV2MissingOrFakeUnlimitedExpiryRejected(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	c := unlimitedCatalog()
	raw, _ := json.Marshal(c)
	for _, replacement := range []string{``, `""`, `"0"`, `"-1"`, `"9999-12-31T23:59:59Z"`} {
		text := string(raw)
		if replacement == `` {
			text = strings.Replace(text, `"subscription_expires_at":null,`, "", 1)
		} else {
			text = strings.Replace(text, `"subscription_expires_at":null`, `"subscription_expires_at":`+replacement, 1)
		}
		var decoded Catalog
		if StrictJSON([]byte(text), &decoded) == nil && decoded.Validate("fixture-only", "registration", now) == nil {
			t.Fatalf("non-null/missing subscription expiry accepted: %q", replacement)
		}
	}
}

func TestVPNAuthV2RequiresExplicitNullInChallengeAndResult(t *testing.T) {
	key, _ := localKey(t)
	identity := VPNIdentity{"test-1", "grant", "registration", "1", "1", "0123456789abcdef", "0", "getconf"}
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "unlimited", true: "mixed-result-rejected"}[mismatch], func(t *testing.T) {
			conn := &recordConn{}
			conn.responder = func(record []byte) [][]byte {
				var id ID
				copy(id[:], record[8:24])
				var env ProofEnvelope
				if err := StrictJSON(record[32:], &env); err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC().Truncate(time.Second)
				var response any
				if env.Op == "VPN_AUTH_BEGIN" {
					response = VPNChallenge{V: 2, Op: "VPN_CHALLENGE", Status: "ok", ChallengeID: EncodeBinary(bytes.Repeat([]byte{1}, 16)), Nonce: EncodeBinary(bytes.Repeat([]byte{2}, 32)), GrantID: identity.GrantID, RegistrationID: identity.RegistrationID, Generation: "1", LeaseSeq: "1", NodeID: identity.NodeID, ChallengeExpiresAt: now.Add(15 * time.Second).Format(time.RFC3339), AccessUnlimited: true, ServerTime: now.Format(time.RFC3339)}
				} else {
					var payload struct {
						Op string `json:"op"`
						VPNIdentity
					}
					if err := StrictJSON(unb64(t, env.PayloadB64), &payload); err != nil {
						t.Fatal(err)
					}
					response = VPNOK{V: 2, Op: "VPN_AUTH_OK", Status: "ok", ChallengeID: env.ChallengeID, VPNIdentity: payload.VPNIdentity, AccessUnlimited: true, ServerTime: now.Format(time.RFC3339)}
					if mismatch {
						response = VPNOK{V: 1, Op: "VPN_AUTH_OK", Status: "ok", ChallengeID: env.ChallengeID, VPNIdentity: payload.VPNIdentity, AccessExpiresAt: now.Add(time.Minute).Format(time.RFC3339), ServerTime: now.Format(time.RFC3339)}
					}
				}
				raw, _ := json.Marshal(response)
				frames, err := Frames(id, true, raw)
				if err != nil {
					t.Fatal(err)
				}
				return frames
			}
			ok, err := AuthenticateVPN(context.Background(), conn, func(string, []byte, int) ([]byte, error) { return bytes.Repeat([]byte{3}, 32), nil }, func(_ context.Context, body []byte) ([]byte, error) { return sign(t, key, body), nil }, identity)
			if mismatch {
				if err == nil {
					t.Fatal("mixed v1/v2 auth accepted")
				}
				return
			}
			if err != nil || ok == nil || !ok.AccessUnlimited {
				t.Fatalf("v2 unlimited auth failed: %v", err)
			}
		})
	}
}
