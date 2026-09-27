package accountaccess

// Hermetic compatibility checks against the authoritative synthetic server fixtures
// delivered for accepted server 193511c (root artifact). The three fixtures are copied
// byte-identical under go_client/testdata/step036/ and every read verifies the pinned
// sha256 first: a rebuilt or edited fixture aborts the test instead of silently fitting
// the assertion. No server, no network, no credentials.
//
// Two real deltas between the fixture inventory and this package's strict decoders are
// recorded here instead of being papered over:
//  1. the browse fixture carries schema_version "0001_core", which is an ERRONEOUS
//     fixture-only token: the actual accepted server 193511c mobile_catalog.py uses
//     SCHEMA_VERSION "1.0" (engineer received a fixture-only correction). The decoder
//     stays strictly pinned to "1.0"; the erroneous fixture is kept only as provenance
//     and its token must be REJECTED, while the corrected wire body ("1.0") passes;
//  2. the credential fixture's gateway/access key inventory does not list the optional
//     probe / vk_hashes fields, while the accepted pair 253cd8a2 catalog_response.json
//     (the declared contract basis of this package) uses both and is strictly decoded
//     by TestAcceptedCatalogFixtureStrictDecodeAndLeaseSeq. Removing them would reject
//     the accepted pair, so they are kept as documented optional extensions.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
)

const (
	step036BrowseFixtureSHA     = "750ad25b84997c2a6c8e19a7c566e54eaadad7c42bffc0a6a3e2f0626b205937"
	step036CredentialFixtureSHA = "518a2f9605d6b2fcb0bf94fbac37c83e2af41fd2f5a23d9b0a0d7baf72736ae8"
)

// step036Fixture reads one hermetic fixture and verifies its pinned sha256 before any
// decoding: a mismatch is a hard abort, never a fit.
func step036Fixture(t *testing.T, name, wantSHA string) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("step036 fixture: runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "..", "testdata", "step036", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("step036 fixture %s: %v", name, err)
	}
	digest := sha256.Sum256(raw)
	got := hex.EncodeToString(digest[:])
	if got != wantSHA {
		t.Fatalf("step036 fixture %s sha256 mismatch: got %s want %s; aborting", name, got, wantSHA)
	}
	return raw
}

// step036BrowseBody removes ONLY the harness envelope marker. The marker is a fixture
// artifact, not a wire field: the strict decoder must reject the raw bytes with it.
func step036BrowseBody(t *testing.T, raw []byte) []byte {
	t.Helper()
	const marker = "  \"synthetic\": true,\n"
	stripped := bytes.Replace(raw, []byte(marker), nil, 1)
	if bytes.Equal(stripped, raw) {
		t.Fatal("fixture envelope marker not found; fixture envelope changed")
	}
	return stripped
}

// TestStep036BrowseFixtureIsProvenanceOnly feeds the authoritative browse fixture to
// the strict display decoder. The raw body carries the harness-envelope key `synthetic`
// and must be rejected by DisallowUnknownFields. After stripping that one envelope
// marker the fixture still carries the erroneous fixture-only schema token "0001_core"
// (actual server 193511c uses "1.0") and must be rejected as well. Only the corrected
// wire body, identical except schema_version "1.0", decodes: 3 gateways, one of them
// (gw-beta) without region/country_code.
func TestStep036BrowseFixtureIsProvenanceOnly(t *testing.T) {
	raw := step036Fixture(t, "step036_catalog_browse.json", step036BrowseFixtureSHA)
	if !bytes.Contains(raw, []byte("\"synthetic\": true")) {
		t.Fatal("fixture envelope marker missing from the raw artifact")
	}

	// Strictness is preserved: the envelope marker is not wire material.
	if _, err := DecodeBrowseStrict(raw); err == nil {
		t.Fatal("raw fixture with the synthetic envelope marker must be rejected")
	}
	if _, err := DecodeGatewaysStrict(raw); err == nil {
		t.Fatal("union accepted the raw fixture with the synthetic envelope marker")
	}

	// Fixture provenance: the delivered token "0001_core" is erroneous material and
	// must not be accepted by the product decoder.
	body := step036BrowseBody(t, raw)
	if _, err := DecodeBrowseStrict(body); err == nil {
		t.Fatal("erroneous fixture-only schema_version 0001_core must be rejected")
	}

	// The actual wire semantics: same body with the server schema "1.0".
	corrected := bytes.Replace(body, []byte(`"schema_version": "0001_core"`), []byte(`"schema_version": "1.0"`), 1)
	if bytes.Equal(corrected, body) {
		t.Fatal("fixture schema token not found; fixture material changed")
	}
	browse, err := DecodeBrowseStrict(corrected)
	if err != nil {
		t.Fatalf("corrected wire-schema browse body rejected: %v", err)
	}
	if browse.SchemaVersion != SchemaVersion {
		t.Fatalf("browse schema_version = %q, want %q", browse.SchemaVersion, SchemaVersion)
	}
	if browse.CatalogMode != CatalogModeBrowse || len(browse.Gateways) != 3 {
		t.Fatalf("browse projection wrong: %+v", browse)
	}
	alpha, beta, gamma := browse.Gateways[0], browse.Gateways[1], browse.Gateways[2]
	if alpha.GatewayID != "gw-alpha" || alpha.Region == nil || *alpha.Region != "test" ||
		alpha.CountryCode == nil || *alpha.CountryCode != "XX" {
		t.Fatalf("gw-alpha metadata wrong: %+v", alpha)
	}
	// gw-beta omits region and country_code entirely: both stay optional, exactly like
	// the Go credential decode.
	if beta.GatewayID != "gw-beta" || beta.Region != nil || beta.CountryCode != nil {
		t.Fatalf("gw-beta must decode with absent region/country_code: %+v", beta)
	}
	if gamma.GatewayID != "gw-gamma" || gamma.Region == nil || *gamma.Region != "eu" ||
		gamma.CountryCode == nil || *gamma.CountryCode != "DE" {
		t.Fatalf("gw-gamma metadata wrong: %+v", gamma)
	}

	union, err := DecodeGatewaysStrict(corrected)
	if err != nil || union.Browse == nil || union.Catalog != nil || len(union.Browse.Gateways) != 3 {
		t.Fatalf("browse union wrong: %+v %v", union, err)
	}

	// Extended wire-error semantics: stripping the marker does not open the union.
	mutated := bytes.Replace(corrected, []byte(`"country_code": "XX"`), []byte(`"country_code": "XX", "revision": "4"`), 1)
	if _, err := DecodeBrowseStrict(mutated); err == nil {
		t.Fatal("browse accepted a credential revision after marker removal")
	}
}

type step036CredentialFixture struct {
	TopLevelKeys  []string       `json:"top_level_keys"`
	GatewayKeys   []string       `json:"gateway_keys"`
	TransportKeys []string       `json:"transport_keys"`
	AccessKeys    []string       `json:"access_keys"`
	Sample        map[string]any `json:"sample"`
}

func structJSONTags(typ reflect.Type) []string {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	tags := make([]string, 0, typ.NumField())
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		tags = append(tags, name)
	}
	sort.Strings(tags)
	return tags
}

// assertJSONTagSet asserts the decoder tag set is EXACTLY the fixture inventory once the
// named documented extensions are accounted for. Every extension must be present in the
// decoder and is logged with its provenance: the delta is recorded, never silent.
func assertJSONTagSet(t *testing.T, subject string, inventory []string, extensions map[string]string, typ reflect.Type) {
	t.Helper()
	remaining := structJSONTags(typ)
	for extension, provenance := range extensions {
		index := slices.Index(remaining, extension)
		if index < 0 {
			t.Fatalf("%s: documented accepted-contract extension %q missing from the decoder tags", subject, extension)
		}
		remaining = slices.Delete(remaining, index, index+1)
		t.Logf("%s: decoder keeps accepted-contract optional field %q absent from the 193511c fixture inventory (%s)",
			subject, extension, provenance)
	}
	want := append([]string(nil), inventory...)
	sort.Strings(want)
	if !slices.Equal(want, remaining) {
		t.Fatalf("%s key inventory mismatch: fixture=%v decoder=%v (report this delta, do not rename wire fields)",
			subject, want, remaining)
	}
}

// TestStep036CredentialFixtureKeyInventoriesMatchStrictDecoders pins the wire key
// inventories of the accepted server 193511c credential response against this package's
// struct tags.
func TestStep036CredentialFixtureKeyInventoriesMatchStrictDecoders(t *testing.T) {
	raw := step036Fixture(t, "step036_catalog_credential.json", step036CredentialFixtureSHA)
	var fixture step036CredentialFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("credential fixture parse: %v", err)
	}
	for name, keys := range map[string][]string{
		"top_level_keys": fixture.TopLevelKeys, "gateway_keys": fixture.GatewayKeys,
		"transport_keys": fixture.TransportKeys, "access_keys": fixture.AccessKeys} {
		if len(keys) == 0 {
			t.Fatalf("credential fixture %s inventory missing", name)
		}
	}

	assertJSONTagSet(t, "CatalogResponse", fixture.TopLevelKeys, nil, reflect.TypeOf(CatalogResponse{}))
	assertJSONTagSet(t, "transportDescriptor", fixture.TransportKeys, nil, reflect.TypeOf(transportDescriptor{}))
	// Recorded delta 2: probe / vk_hashes are accepted optional extensions of the
	// declared contract basis (accepted pair 253cd8a2) and are absent from this fixture
	// inventory. They are kept, documented, never removed.
	assertJSONTagSet(t, "gateway", fixture.GatewayKeys, map[string]string{
		"probe": "accepted pair 253cd8a2 catalog_response.json",
	}, reflect.TypeOf(gateway{}))
	assertJSONTagSet(t, "accessDescriptor", fixture.AccessKeys, map[string]string{
		"vk_hashes": "accepted pair 253cd8a2 catalog_response.json",
	}, reflect.TypeOf(accessDescriptor{}))
}

// TestStep036CredentialFixtureSampleBuildsAcceptedEnvelope projects the fixture `sample`
// gateway into a full credential envelope (required envelope fields, decimal revision)
// and verifies the strict union accepts it. The fixture spki is the literal placeholder
// "synthetic-pin", which is not a 32-byte canonical base64url pin: that single value is
// fixture placeholder material (proven by the negative case below), not a wire delta.
func TestStep036CredentialFixtureSampleBuildsAcceptedEnvelope(t *testing.T) {
	raw := step036Fixture(t, "step036_catalog_credential.json", step036CredentialFixtureSHA)
	var fixture step036CredentialFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("credential fixture parse: %v", err)
	}
	if fixture.Sample == nil {
		t.Fatal("credential fixture sample gateway missing")
	}
	cloneSample := func() map[string]any {
		encoded, err := json.Marshal(fixture.Sample)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(encoded, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	envelope := func(node map[string]any) []byte {
		encoded, err := json.Marshal(map[string]any{
			"request_id":     "0123456789abcdef0123456789abcdef",
			"server_time":    "2026-09-23T12:00:00Z",
			"schema_version": SchemaVersion,
			"status":         "ok",
			"revision":       "7",
			"valid_until":    "2026-09-23T12:10:00Z",
			"issued_at":      "2026-09-23T12:00:00Z",
			"gateways":       []any{node},
		})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}

	if _, err := DecodeGatewaysStrict(envelope(cloneSample())); err == nil {
		t.Fatal("the synthetic-pin placeholder must not satisfy the strict spki rule")
	}

	gateway := cloneSample()
	transport, ok := gateway["transport"].(map[string]any)
	if !ok {
		t.Fatalf("sample transport wrong: %v", gateway["transport"])
	}
	transport["dtls_spki_sha256"] = "PAyFy4YlbMsIfA4GfHvX_r8-HUWKVuj9PUeJ4pEtAjo"

	union, err := DecodeGatewaysStrict(envelope(gateway))
	if err != nil {
		t.Fatalf("accepted credential envelope rejected: %v", err)
	}
	if union.Catalog == nil || union.Browse != nil || len(union.Catalog.Gateways) != 1 {
		t.Fatalf("credential union wrong: %+v", union)
	}
	if union.Catalog.Revision != "7" || union.Catalog.SchemaVersion != SchemaVersion {
		t.Fatalf("credential envelope projection wrong: %+v", union.Catalog)
	}
	decoded := union.Catalog.Gateways[0]
	if decoded.GatewayID != "gw-alpha" || decoded.Name != "ALPHA" || decoded.Region == nil || *decoded.Region != "test" ||
		decoded.CountryCode == nil || *decoded.CountryCode != "XX" || decoded.TargetWorkers == nil || *decoded.TargetWorkers != 36 ||
		!reflect.DeepEqual(decoded.Capabilities, []string{"managed"}) || decoded.Probe != nil || decoded.Access.VKHashes != nil {
		t.Fatalf("sample gateway projection wrong: %+v", decoded)
	}
	if decoded.Transport.Protocol != "wdtt-v17" || decoded.Transport.PeerIP != "127.0.0.1" ||
		decoded.Transport.DTLSPort != 56300 || decoded.Transport.WGPort != 56302 ||
		decoded.Transport.DTLSSPKISHA256 != transport["dtls_spki_sha256"] {
		t.Fatalf("sample transport projection wrong: %+v", decoded.Transport)
	}
	if decoded.Access.GrantID != "00000000-0000-4000-8000-000000000000" ||
		decoded.Access.DeviceRef != "synthetic-fingerprint" || decoded.Access.Password != "synthetic-password" ||
		decoded.Access.Generation != "1" || decoded.Access.LeaseSeq != "1" ||
		decoded.Access.NotAfter != "2026-09-23T13:00:00Z" {
		t.Fatalf("sample access projection wrong: %+v", decoded.Access)
	}
}
