package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
	"wg-turn-client/accountaccess"
	"wg-turn-client/wlbs"
)

func TestMobileSelectionRestoreWiring(t *testing.T) {
	for _, kind := range []string{"restore_A", "removed_A", "empty", "denied_A", "other_account", "other_installation", "other_source", "browse_clears"} {
		t.Run(kind, func(t *testing.T) {
			f := newMobileFeedFixture(t, 2, mobileFeedOptions{})
			me, cat := feedDecodeMe(t, f.me), feedDecodeCatalog(t, f)
			makeAttempt := func(pref *mobileSelectionPreference) (*managedController, *managedMobile, *feedHost) {
				t.Helper()
				// Strictly decode the actual optional start field; saved selection starts empty.
				raw, _ := json.Marshal(map[string]any{"mobile_base_url": "https://unused.invalid", "mobile_environment": "test", "mobile_selection": pref})
				var start managedStart
				if err := wlbs.StrictJSON(raw, &start); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				host := &feedHost{}
				c := &managedController{start: start}
				c.bridge = newManagedBridge(host, "restore", cancel)
				m := &managedMobile{controller: c, bridge: c.bridge, fingerprint: f.fingerprint, runCtx: ctx, verifiedSig: make(chan struct{}, 1)}
				c.mobile = m
				t.Cleanup(func() { m.retireProbeAdmission(); m.mu.Lock(); m.stopTimerLocked(); m.mu.Unlock() })
				return c, m, host
			}
			selectedFrame := func(h *feedHost) string {
				t.Helper()
				h.mu.Lock()
				defer h.mu.Unlock()
				selected := "missing"
				for _, frame := range h.messages {
					if frame["type"] != "catalog" {
						t.Fatalf("unexpected side effect frame %v", frame["type"])
					}
					if frame["catalog_mode"] != "browse" {
						selected, _ = frame["selected_node_id"].(string)
					}
				}
				return selected
			}
			// Real manual choose -> save -> catalog callback acknowledges A without Connect.
			previous, previousMobile, oldHost := makeAttempt(nil)
			previousMobile.onVerified(me, cat)
			if err := previous.chooseNode(context.Background(), "gw-1"); err != nil {
				t.Fatal(err)
			}
			if err := previous.publishCatalogSnapshotContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			if selectedFrame(oldHost) != "gw-1" {
				t.Fatal("manual selection not acknowledged")
			}
			account := ""
			if me.AccountRef != nil {
				account = *me.AccountRef
			}
			pref := &mobileSelectionPreference{NodeID: "gw-1", InstallationID: f.fingerprint, BaseURL: "https://unused.invalid", Environment: "test", AccountRef: account}
			want := ""
			switch kind {
			case "restore_A":
				want = "gw-1"
			case "removed_A":
				cat.Gateways = cat.Gateways[:1]
			case "empty":
				pref = nil
			case "denied_A":
				me.AccountState = "REVOKED_SESSION"
			case "other_account":
				pref.AccountRef = "different"
			case "other_installation":
				pref.InstallationID = "different"
			case "other_source":
				pref.BaseURL = "https://other.invalid"
			}
			c, m, host := makeAttempt(pref)
			if c.selectionID() != "" || c.store.Snapshot() != nil {
				t.Fatal("preference became authority before fresh pair")
			}
			if kind == "browse_clears" {
				m.onBrowse(accountaccess.BrowseCatalogResponse{})
			}
			m.onVerified(me, cat)
			if m.verifiedErr != nil {
				t.Fatal(m.verifiedErr)
			}
			if c.selectionID() != want || selectedFrame(host) != want {
				t.Fatalf("native/host selection want=%q got=%q frame=%q", want, c.selectionID(), selectedFrame(host))
			}
			if want != "" && c.store.Snapshot().ValidateNode("", "", want, time.Now()) != nil {
				t.Fatal("restored selection lacks current proof")
			}
			if c.start.MobileSelection != nil {
				t.Fatal("preference not consumed")
			}
			fresh := feedDecodeCatalog(t, f)
			freshMe := feedDecodeMe(t, f.me)
			m.onVerified(freshMe, fresh)
			if c.selectionID() != want || selectedFrame(host) != want {
				t.Fatal("rejected preference revived")
			}
			// The acknowledged empty ID removes the host preference; another restart stays empty.
			next := (*mobileSelectionPreference)(nil)
			if want != "" {
				copy := *pref
				next = &copy
			}
			restarted, restartedMobile, restartedHost := makeAttempt(next)
			restartedMobile.onVerified(freshMe, fresh)
			if restarted.selectionID() != want || selectedFrame(restartedHost) != want {
				t.Fatal("restart changed selection")
			}
			if host.persists != 0 || oldHost.persists != 0 {
				t.Fatal("legacy state persisted in mobile mode")
			}
		})
	}
}
