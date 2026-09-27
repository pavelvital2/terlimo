package wlbs

import (
	"encoding/json"
	"math/big"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

type Endpoint struct {
	NodeID         string `json:"node_id"`
	PeerIP         string `json:"peer_ip"`
	DTLSPort       int    `json:"dtls_port"`
	DTLSSPKISHA256 string `json:"dtls_spki_sha256"`
}
type Link struct {
	V               int        `json:"v"`
	Env             string     `json:"env"`
	SubscriptionRef string     `json:"subscription_ref"`
	CredentialID    string     `json:"credential_id"`
	BootstrapSecret string     `json:"bootstrap_secret"`
	Bootstrap       []Endpoint `json:"bootstrap"`
	VKHashes        []string   `json:"vk_hashes"`
	IssuedAt        string     `json:"issued_at"`
	ExpiresAt       string     `json:"expires_at,omitempty"`
}
type LinkEnvelope struct {
	Kid          string `json:"kid"`
	PayloadB64   string `json:"payload_b64"`
	SignatureB64 string `json:"signature_b64"`
}

func UTC(s string) (time.Time, error) {
	if !strings.HasSuffix(s, "Z") {
		return time.Time{}, failure("BAD_MESSAGE")
	}
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return time.Time{}, failure("BAD_MESSAGE")
	}
	return t, nil
}
func validEndpoint(e Endpoint) bool {
	_, err := DecodeBinary(e.DTLSSPKISHA256, 32)
	return e.NodeID != "" && net.ParseIP(e.PeerIP) != nil && e.DTLSPort > 0 && e.DTLSPort <= 65535 && err == nil
}
func nonempty(items []string) bool {
	if len(items) == 0 {
		return false
	}
	for _, s := range items {
		if s == "" {
			return false
		}
	}
	return true
}
func VerifyLink(raw string, issuers map[string][]byte, expectedEnv string, now time.Time) (*Link, error) {
	if len(raw) > 32768 {
		return nil, failure("TRUST_FAILED")
	}
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "whitelists" || u.Host != "subscription" || u.Path != "" || u.RawQuery != "v=1" || u.User != nil || u.Fragment == "" {
		return nil, failure("TRUST_FAILED")
	}
	b, e := DecodeBinary(u.Fragment, -1)
	if e != nil {
		return nil, failure("TRUST_FAILED")
	}
	var env LinkEnvelope
	if StrictJSON(b, &env) != nil {
		return nil, failure("TRUST_FAILED")
	}
	key, ok := issuers[env.Kid]
	if !ok {
		return nil, failure("TRUST_FAILED")
	}
	p, e := DecodeBinary(env.PayloadB64, -1)
	if e != nil {
		return nil, failure("TRUST_FAILED")
	}
	sig, e := DecodeBinary(env.SignatureB64, -1)
	if e != nil || VerifyTranscript(key, LinkTranscript(p), sig) != nil {
		return nil, failure("TRUST_FAILED")
	}
	var link Link
	if StrictJSON(p, &link) != nil || link.V != 1 || link.Env != expectedEnv || link.SubscriptionRef == "" || link.CredentialID == "" || link.BootstrapSecret == "" || len(link.Bootstrap) != 1 || !validEndpoint(link.Bootstrap[0]) || !nonempty(link.VKHashes) {
		return nil, failure("TRUST_FAILED")
	}
	issued, e := UTC(link.IssuedAt)
	if e != nil || issued.After(now.Add(time.Minute)) {
		return nil, failure("TRUST_FAILED")
	}
	if link.ExpiresAt != "" {
		expires, e := UTC(link.ExpiresAt)
		if e != nil || !expires.After(now) || !expires.After(issued) {
			return nil, failure("TRUST_FAILED")
		}
	}
	return &link, nil
}
func CompareDecimal(a, b string) (int, error) {
	parse := func(s string) (*big.Int, bool) {
		if s == "" || len(s) > 128 || (len(s) > 1 && s[0] == '0') {
			return nil, false
		}
		for _, r := range s {
			if r < '0' || r > '9' {
				return nil, false
			}
		}
		return new(big.Int).SetString(s, 10)
	}
	aa, ok := parse(a)
	if !ok {
		return 0, failure("BAD_MESSAGE")
	}
	bb, ok := parse(b)
	if !ok {
		return 0, failure("BAD_MESSAGE")
	}
	return aa.Cmp(bb), nil
}

type Access struct {
	GrantID   string   `json:"grant_id"`
	DeviceID  string   `json:"device_id"`
	Password  string   `json:"password"`
	VKHashes  []string `json:"vk_hashes"`
	ExpiresAt string   `json:"expires_at"`
	// Unlimited is true only when an explicit JSON null was decoded. An omitted
	// expires_at is invalid and never acquires this meaning.
	Unlimited  bool   `json:"-"`
	Generation string `json:"generation"`
	LeaseSeq   string `json:"lease_seq"`
}

func decodeNullableString(data []byte, key string, out any) (bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false, err
	}
	raw, ok := fields[key]
	if !ok {
		return false, json.Unmarshal(data, out)
	}
	unlimited := string(raw) == "null"
	if unlimited {
		fields[key] = json.RawMessage(`""`)
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		return false, err
	}
	return unlimited, json.Unmarshal(normalized, out)
}

func marshalNullableString(value any, key string, unlimited bool) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil || !unlimited {
		return raw, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields[key] = json.RawMessage("null")
	return json.Marshal(fields)
}

func (a *Access) UnmarshalJSON(data []byte) error {
	type alias Access
	var decoded alias
	unlimited, err := decodeNullableString(data, "expires_at", &decoded)
	if err != nil {
		return err
	}
	decoded.Unlimited = unlimited
	*a = Access(decoded)
	return nil
}

func (a Access) MarshalJSON() ([]byte, error) {
	type alias Access
	return marshalNullableString(alias(a), "expires_at", a.Unlimited)
}

func (a Access) ValidAt(now time.Time) bool {
	if a.Unlimited {
		return true
	}
	expires, err := UTC(a.ExpiresAt)
	return err == nil && expires.After(now)
}

type Node struct {
	Endpoint
	Name        string `json:"name"`
	CountryCode string `json:"country_code"`
	WGPort      int    `json:"wg_port"`
	Protocol    string `json:"protocol"`
	AuthMode    string `json:"auth_mode"`
	MaxWorkers  int    `json:"max_workers"`
	Access      Access `json:"access"`
}

var errCountryCodeWire = &Error{Code: "BAD_CATALOG"}

// UnmarshalJSON distinguishes an explicitly unknown country ("") from a
// missing, null or non-string wire field. Constructed Nodes remain governed by
// Catalog.Validate's existing empty-or-two-byte semantic check.
func (n *Node) UnmarshalJSON(data []byte) error {
	var field struct {
		CountryCode json.RawMessage `json:"country_code"`
	}
	if err := json.Unmarshal(data, &field); err != nil {
		return err
	}
	if len(field.CountryCode) == 0 {
		return errCountryCodeWire
	}
	var country *string
	if err := json.Unmarshal(field.CountryCode, &country); err != nil || country == nil {
		return errCountryCodeWire
	}
	type nodeAlias Node
	var decoded nodeAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	decoded.CountryCode = *country
	*n = Node(decoded)
	return nil
}

// MobileCatalogProof is the typed provenance of a catalog snapshot accepted by the
// mobile coordinator from a verified /me + /gateways pair. It is internal: JSON
// restore never reconstructs it, so disk/network data is untrusted by default and a
// fresh validated coordinator has to prove admission again.
type MobileCatalogProof struct {
	NodeID              string
	Subject             string
	CatalogRevision     string
	GrantID             string
	Generation          string
	LeaseSeq            string
	NotAfter            time.Time
	CatalogValidUntil   time.Time
	EntitlementDeadline time.Time
	TargetWorkers       int
	VerifiedAt          time.Time
}

type Catalog struct {
	V                     int `json:"v"`
	mobileProof           *MobileCatalogProof
	Status                string `json:"status"`
	SubscriptionRef       string `json:"subscription_ref"`
	RegistrationID        string `json:"registration_id"`
	SubscriptionStatus    string `json:"subscription_status"`
	SubscriptionExpiresAt string `json:"subscription_expires_at"`
	SubscriptionUnlimited bool   `json:"-"`
	SlotsLimit            int    `json:"slots_limit"`
	SlotsUsed             int    `json:"slots_used"`
	Revision              string `json:"revision"`
	IssuedAt              string `json:"issued_at"`
	RefreshAfter          string `json:"refresh_after"`
	CatalogExpiresAt      string `json:"catalog_expires_at"`
	ServerTime            string `json:"server_time,omitempty"`
	Nodes                 []Node `json:"nodes"`
}

func (c *Catalog) UnmarshalJSON(data []byte) error {
	type alias Catalog
	var decoded alias
	unlimited, err := decodeNullableString(data, "subscription_expires_at", &decoded)
	if err != nil {
		return err
	}
	decoded.SubscriptionUnlimited = unlimited
	*c = Catalog(decoded)
	return nil
}

func (c Catalog) MarshalJSON() ([]byte, error) {
	type alias Catalog
	return marshalNullableString(alias(c), "subscription_expires_at", c.SubscriptionUnlimited)
}

func (c *Catalog) Validate(subscription, registration string, now time.Time) error {
	return c.validate(subscription, registration, now, true)
}

// WithMobileProof binds the catalog snapshot to an explicit typed mobile provenance.
// Only the mobile entrypoint constructs it; legacy catalogs keep nil and stay strict.
func (c *Catalog) WithMobileProof(proof MobileCatalogProof) *Catalog {
	copied := *c
	copied.mobileProof = &proof
	return &copied
}

// MobileAuthority reports whether this snapshot carries mobile provenance.
func (c *Catalog) MobileAuthority() bool { return c != nil && c.mobileProof != nil }

// ValidateNode separates selected-grant admission from fresh catalog metadata.
// A valid catalog retains expired grants, but only a selected grant with a
// current lease may authorize a tunnel.
func (c *Catalog) ValidateNode(subscription, registration, nodeID string, now time.Time) error {
	if e := c.Validate(subscription, registration, now); e != nil {
		return e
	}
	if c.mobileProof != nil && c.mobileProof.NodeID != nodeID {
		return failure("SELECTED_NODE_REMOVED")
	}
	for _, n := range c.Nodes {
		if n.NodeID != nodeID {
			continue
		}
		if !n.Access.ValidAt(now) {
			return failure("BAD_CATALOG")
		}
		return nil
	}
	return failure("SELECTED_NODE_REMOVED")
}

// ValidateRefreshMetadata permits an expired catalog timestamp as input to
// authenticated recovery. It never authorizes a tunnel. Subscription, schema,
// endpoint and key checks remain mandatory; expired grants remain metadata.
func (c *Catalog) ValidateRefreshMetadata(subscription, registration string, now time.Time) error {
	return c.validate(subscription, registration, now, false)
}

func (c *Catalog) validate(subscription, registration string, now time.Time, fresh bool) error {
	if c != nil && c.mobileProof != nil {
		return c.validateMobile(now)
	}
	if c == nil || (c.V != 1 && c.V != 2) || c.Status != "ok" || c.SubscriptionRef != subscription || c.SubscriptionRef == "" || c.RegistrationID == "" || (registration != "" && c.RegistrationID != registration) || c.SubscriptionStatus != "active" || c.SlotsLimit < 1 || c.SlotsUsed < 1 || c.SlotsUsed > c.SlotsLimit || len(c.Nodes) < 1 || len(c.Nodes) > 2 || (c.V == 1 && c.SubscriptionUnlimited) || (c.V == 2 && !c.SubscriptionUnlimited) {
		return failure("BAD_CATALOG")
	}
	if _, e := CompareDecimal(c.Revision, "0"); e != nil {
		return failure("BAD_CATALOG")
	}
	issued, e := UTC(c.IssuedAt)
	if e != nil {
		return failure("BAD_CATALOG")
	}
	refresh, e := UTC(c.RefreshAfter)
	if e != nil {
		return failure("BAD_CATALOG")
	}
	expires, e := UTC(c.CatalogExpiresAt)
	if e != nil {
		return failure("BAD_CATALOG")
	}
	var sub time.Time
	if !c.SubscriptionUnlimited {
		sub, e = UTC(c.SubscriptionExpiresAt)
	}
	if e != nil || (fresh && !expires.After(now)) || !expires.After(issued) || (!c.SubscriptionUnlimited && expires.After(sub)) || expires.Sub(issued) > 15*time.Minute || refresh.Before(issued) || refresh.After(expires) || issued.After(now.Add(time.Minute)) {
		return failure("BAD_CATALOG")
	}
	if !fresh && !c.SubscriptionUnlimited && !sub.After(now) {
		return failure("SUBSCRIPTION_EXPIRED")
	}
	nodeIDs := make(map[string]struct{}, len(c.Nodes))
	grantIDs := make(map[string]struct{}, len(c.Nodes))
	for _, n := range c.Nodes {
		a := n.Access
		if _, duplicate := nodeIDs[n.NodeID]; duplicate {
			return failure("BAD_CATALOG")
		}
		if _, duplicate := grantIDs[a.GrantID]; duplicate {
			return failure("BAD_CATALOG")
		}
		nodeIDs[n.NodeID] = struct{}{}
		grantIDs[a.GrantID] = struct{}{}
		if !validEndpoint(n.Endpoint) || n.Name == "" || (n.CountryCode != "" && len(n.CountryCode) != 2) || n.Protocol != "wdtt-v17" || n.AuthMode != "installation-pop-v1" || n.WGPort < 1 || n.WGPort > 65535 || n.MaxWorkers < 1 || a.GrantID == "" || a.DeviceID != c.RegistrationID || a.Password == "" || !nonempty(a.VKHashes) {
			return failure("BAD_CATALOG")
		}
		if _, e := CompareDecimal(a.Generation, "0"); e != nil {
			return failure("BAD_CATALOG")
		}
		if _, e := CompareDecimal(a.LeaseSeq, "0"); e != nil {
			return failure("BAD_CATALOG")
		}
		if (c.V == 1 && a.Unlimited) || (c.V == 2 && !a.Unlimited) {
			return failure("BAD_CATALOG")
		}
		lease, e := UTC(a.ExpiresAt)
		if a.Unlimited {
			e = nil
		}
		if e != nil || (!a.Unlimited && ((!c.SubscriptionUnlimited && lease.After(sub)) || lease.Sub(issued) > 15*time.Minute)) {
			return failure("BAD_CATALOG")
		}
	}
	return nil
}

// validateMobile applies only the shared technical checks to a mobile-authority
// snapshot: no legacy business envelope (subscription_ref/status/slots/vk_hashes) and
// no legacy 15-minute lease rule; deadlines come from the proven mobile proof.
func (c *Catalog) validateMobile(now time.Time) error {
	proof := c.mobileProof
	if c.V != 2 || c.Status != "ok" || c.SubscriptionUnlimited || c.RegistrationID == "" ||
		c.SubscriptionRef == "" || c.RegistrationID != proof.Subject || len(c.Nodes) < 1 ||
		proof.Subject == "" || proof.NodeID == "" || proof.CatalogRevision == "" || c.Revision != proof.CatalogRevision {
		return failure("BAD_CATALOG")
	}
	for _, value := range []string{c.Revision, proof.Generation, proof.LeaseSeq} {
		if _, e := CompareDecimal(value, "0"); e != nil {
			return failure("BAD_CATALOG")
		}
	}
	issued, e := UTC(c.IssuedAt)
	if e != nil {
		return failure("BAD_CATALOG")
	}
	expires, e := UTC(c.CatalogExpiresAt)
	if e != nil || !expires.After(now) || !expires.After(issued) {
		return failure("BAD_CATALOG")
	}
	// Deadline source is the proven mobile pair, never the legacy 15-minute envelope.
	// The catalog validity fence travels inside the proof so clone/store cannot extend it.
	if proof.EntitlementDeadline.IsZero() || proof.NotAfter.IsZero() || proof.CatalogValidUntil.IsZero() ||
		!proof.NotAfter.After(now) || proof.NotAfter.After(proof.EntitlementDeadline) ||
		!proof.CatalogValidUntil.After(now) || proof.CatalogValidUntil.After(proof.EntitlementDeadline) ||
		!expires.Equal(proof.CatalogValidUntil) ||
		expires.After(proof.EntitlementDeadline) || expires.After(proof.NotAfter) ||
		proof.TargetWorkers < 1 {
		return failure("BAD_CATALOG")
	}
	nodeIDs := make(map[string]struct{}, len(c.Nodes))
	grantIDs := make(map[string]struct{}, len(c.Nodes))
	proofNode := false
	for _, n := range c.Nodes {
		a := n.Access
		if _, duplicate := nodeIDs[n.NodeID]; duplicate {
			return failure("BAD_CATALOG")
		}
		if _, duplicate := grantIDs[a.GrantID]; duplicate {
			return failure("BAD_CATALOG")
		}
		nodeIDs[n.NodeID] = struct{}{}
		grantIDs[a.GrantID] = struct{}{}
		if !validEndpoint(n.Endpoint) || n.Name == "" || (n.CountryCode != "" && len(n.CountryCode) != 2) || n.Protocol != "wdtt-v17" || n.AuthMode != "installation-pop-v1" || n.WGPort < 1 || n.WGPort > 65535 || n.MaxWorkers < 1 || n.MaxWorkers != proof.TargetWorkers || a.GrantID == "" || a.DeviceID != c.RegistrationID || a.Password == "" {
			return failure("BAD_CATALOG")
		}
		if _, e := CompareDecimal(a.Generation, "0"); e != nil {
			return failure("BAD_CATALOG")
		}
		if _, e := CompareDecimal(a.LeaseSeq, "0"); e != nil {
			return failure("BAD_CATALOG")
		}
		if a.Unlimited {
			return failure("BAD_CATALOG")
		}
		lease, e := UTC(a.ExpiresAt)
		if e != nil || !lease.After(now) || lease.After(proof.EntitlementDeadline) {
			return failure("BAD_CATALOG")
		}
		if n.NodeID == proof.NodeID {
			proofNode = true
			if a.GrantID != proof.GrantID || a.Generation != proof.Generation || a.LeaseSeq != proof.LeaseSeq ||
				!lease.Equal(proof.NotAfter) {
				return failure("BAD_CATALOG")
			}
		}
	}
	if !proofNode {
		return failure("SELECTED_NODE_REMOVED")
	}
	return nil
}

// CatalogStore replaces full snapshots atomically. not_modified never changes
// expiry, revision or grants. Callers own secure persistence, not this library.
type CatalogStore struct {
	mu      sync.RWMutex
	catalog *Catalog
}

func cloneCatalog(c *Catalog) *Catalog {
	if c == nil {
		return nil
	}
	b, _ := json.Marshal(c)
	var out Catalog
	_ = json.Unmarshal(b, &out)
	// Typed mobile provenance is internal and survives cloning; it is never part of the
	// serialized shape, so JSON restore cannot resurrect trust.
	out.mobileProof = c.mobileProof
	return &out
}
func (s *CatalogStore) Snapshot() *Catalog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneCatalog(s.catalog)
}
func (s *CatalogStore) Apply(c *Catalog, subscription, registration string, now time.Time) error {
	if e := c.Validate(subscription, registration, now); e != nil {
		return e
	}
	return s.applyValidated(c)
}

// For recovery stores only; callers must still Apply fresh catalog metadata
// before CatalogReady, and separately ValidateNode before VPN admission.
func (s *CatalogStore) ApplyRefreshMetadata(c *Catalog, subscription, registration string, now time.Time) error {
	if e := c.ValidateRefreshMetadata(subscription, registration, now); e != nil {
		return e
	}
	return s.applyValidated(c)
}

// sameRevisionNodeIdentity compares every material identity/config field of one
// signed node for the same-revision store fence. Access.ExpiresAt is a freshness
// horizon, not identity, and is deliberately excluded. This mirrors the runner's
// sameManagedNodeConfig (package main): the model package cannot import package
// main without an import cycle, and the store fence must stay independent from
// runner admission code, so the small comparison is intentionally duplicated.
func sameRevisionNodeIdentity(a, b Node) bool {
	return a.Endpoint == b.Endpoint && a.Name == b.Name && a.CountryCode == b.CountryCode &&
		a.WGPort == b.WGPort && a.Protocol == b.Protocol && a.AuthMode == b.AuthMode &&
		a.MaxWorkers == b.MaxWorkers && a.Access.GrantID == b.Access.GrantID &&
		a.Access.DeviceID == b.Access.DeviceID && a.Access.Password == b.Access.Password &&
		slices.Equal(a.Access.VKHashes, b.Access.VKHashes) && a.Access.Unlimited == b.Access.Unlimited &&
		a.Access.Generation == b.Access.Generation && a.Access.LeaseSeq == b.Access.LeaseSeq
}

// sameRevisionIdentity reports whether a validated catalog carries the same signed
// business identity as the already stored one. Only freshness/horizon literals
// (IssuedAt, RefreshAfter, CatalogExpiresAt, SubscriptionExpiresAt, ServerTime and
// each node's Access.ExpiresAt) and the internal typed mobile proof are exempt: a
// same-revision re-issue may refresh those without inventing a new revision. The
// revision literal itself, the exact node set and order, and every material field
// stay strict. Shortened or expired horizons are never widened here; admission and
// the proven deadline enforce them fail-closed elsewhere.
func sameRevisionIdentity(c, previous *Catalog) bool {
	if c == nil || previous == nil {
		return false
	}
	if c.V != previous.V || c.Status != previous.Status || c.SubscriptionRef != previous.SubscriptionRef ||
		c.RegistrationID != previous.RegistrationID || c.SubscriptionStatus != previous.SubscriptionStatus ||
		c.SubscriptionUnlimited != previous.SubscriptionUnlimited || c.SlotsLimit != previous.SlotsLimit ||
		c.SlotsUsed != previous.SlotsUsed || c.Revision != previous.Revision || len(c.Nodes) != len(previous.Nodes) {
		return false
	}
	for index := range c.Nodes {
		if !sameRevisionNodeIdentity(c.Nodes[index], previous.Nodes[index]) {
			return false
		}
	}
	return true
}

func (s *CatalogStore) applyValidated(c *Catalog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.catalog != nil {
		// A newer catalog revision cannot roll an existing grant's authorization
		// generation or lease fence backwards.
		for _, previous := range s.catalog.Nodes {
			for _, next := range c.Nodes {
				if next.NodeID != previous.NodeID || next.Access.GrantID != previous.Access.GrantID {
					continue
				}
				generation, e := CompareDecimal(next.Access.Generation, previous.Access.Generation)
				if e != nil || generation < 0 {
					return failure("STALE_CATALOG")
				}
				if generation == 0 {
					seq, e := CompareDecimal(next.Access.LeaseSeq, previous.Access.LeaseSeq)
					if e != nil || seq < 0 {
						return failure("STALE_CATALOG")
					}
				}
			}
		}
		cmp, e := CompareDecimal(c.Revision, s.catalog.Revision)
		if e != nil {
			return e
		}
		if cmp < 0 {
			return failure("STALE_CATALOG")
		}
		for _, old := range s.catalog.Nodes {
			for _, next := range c.Nodes {
				if old.NodeID == next.NodeID && old.Access.GrantID == next.Access.GrantID {
					g, _ := CompareDecimal(next.Access.Generation, old.Access.Generation)
					seq, _ := CompareDecimal(next.Access.LeaseSeq, old.Access.LeaseSeq)
					if g < 0 || (g == 0 && seq < 0) {
						return failure("LEASE_CONFLICT")
					}
				}
			}
		}
		if cmp == 0 {
			if !sameRevisionIdentity(c, s.catalog) {
				return failure("REVISION_CONFLICT")
			}
			// Identity matched: freshness/horizon literals and the typed mobile proof
			// may evolve within one revision. The store is monotonic in IssuedAt: an
			// out-of-order older same-revision response must never replace a newer
			// snapshot and re-extend a shortened catalog/subscription/node deadline.
			// Equal timestamps keep the previous install behavior (idempotent
			// re-issue / mobile proof re-bind); a strictly newer snapshot installs
			// extension and shortening alike. A shortened or expired horizon is
			// stored as-is and stays fail-closed in admission and the proven deadline,
			// never widened here. The proof is admission metadata, not catalog business
			// content: a mobile selection change re-binds the same verified revision to
			// another selected node without inventing a new revision.
			incomingIssued, incomingErr := UTC(c.IssuedAt)
			storedIssued, storedErr := UTC(s.catalog.IssuedAt)
			if incomingErr != nil || storedErr != nil {
				// Defensive fail-safe: every validated catalog carries a parseable
				// IssuedAt, so this branch is unreachable through Apply/
				// ApplyRefreshMetadata. If that invariant is ever broken by an
				// internal caller, keep the known-good stored snapshot rather than
				// let an unorderable response replace it.
				return nil
			}
			if incomingIssued.Before(storedIssued) {
				// Accept the call without error but keep the newer stored snapshot.
				return nil
			}
			s.catalog = cloneCatalog(c)
			return nil
		}
	}
	s.catalog = cloneCatalog(c)
	return nil
}
func (s *CatalogStore) NotModified(revision string, now time.Time) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.catalog == nil || revision != s.catalog.Revision {
		return failure("STALE_CATALOG")
	}
	end, e := UTC(s.catalog.CatalogExpiresAt)
	if e != nil || !end.After(now) {
		return failure("CATALOG_EXPIRED")
	}
	return nil
}
