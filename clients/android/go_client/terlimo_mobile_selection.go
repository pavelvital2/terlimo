package main

import "wg-turn-client/accountaccess"

// Only an ID preference from installation-encrypted host storage, never cached authority.
type mobileSelectionPreference struct {
	NodeID         string `json:"node_id"`
	InstallationID string `json:"installation_id"`
	BaseURL        string `json:"base_url"`
	Environment    string `json:"environment"`
	AccountRef     string `json:"account_ref"`
}

func (m *managedMobile) selectionScopeMatches(preference *mobileSelectionPreference, me accountaccess.MeResponse) bool {
	environment := m.controller.start.MobileEnvironment
	if environment == "" {
		environment = "test"
	}
	account := ""
	if me.AccountRef != nil {
		account = *me.AccountRef
	}
	return preference.NodeID != "" && len(preference.NodeID) <= 200 &&
		preference.InstallationID == m.fingerprint &&
		preference.BaseURL == m.controller.start.MobileBaseURL &&
		preference.Environment == environment && preference.AccountRef == account
}

// Metadata may disprove membership, but cannot admit a dormant preference.
// Leave an already acknowledged selection alone while its subject/ID still match;
// the existing Runner admission callback owns revocation of its current runtime.
func (m *managedMobile) reconcileBrowseSelection(browse accountaccess.BrowseCatalogResponse) {
	m.mu.Lock()
	current, verified := m.latestMe, m.me
	m.mu.Unlock()
	present := func(id string) bool {
		for _, gateway := range browse.Gateways {
			if gateway.GatewayID == id {
				return true
			}
		}
		return false
	}
	c := m.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.start.MobileSelection; p != nil &&
		(!present(p.NodeID) || (current != nil && !m.selectionScopeMatches(p, *current))) {
		c.start.MobileSelection = nil
	}
	if id := c.saved.SelectedNodeID; id != "" {
		sameAccount := current == nil || verified == nil ||
			(current.AccountRef == nil && verified.AccountRef == nil) ||
			(current.AccountRef != nil && verified.AccountRef != nil && *current.AccountRef == *verified.AccountRef)
		if !present(id) || !sameAccount {
			c.saved.SelectedNodeID = ""
		}
	}
}

// Consume once on the first fresh pair. A rejected/removed preference cannot revive
// on a later pair in this attempt; its empty catalog selection clears the host copy.
func (m *managedMobile) takeSelectionPreference(me accountaccess.MeResponse, catalog accountaccess.CatalogResponse) string {
	c := m.controller
	c.mu.Lock()
	preference := c.start.MobileSelection
	c.start.MobileSelection = nil
	selected := c.saved.SelectedNodeID
	c.mu.Unlock()
	if preference == nil || selected != "" {
		return ""
	}
	if !m.selectionScopeMatches(preference, me) {
		return ""
	}
	if !accountaccess.DecideAdmission(me, catalog, preference.NodeID, m.now()).Admitted {
		return ""
	}
	return preference.NodeID
}
