# Provenance — accepted-0503391 (contract pack 0503391)

- Contract/pack id: `050339123bb77d1ebda866000d2498e3be5a9b77` (as delivered with the relay
  task; the id string itself does not appear inside the archive).
- Delivery archive: `step035-contract-0503391.tar.gz`
- Archive sha256: `d2c05876c38eb69033e2c2386ef97ead981a54b8b9d798a9efe586f42e3d3e92` (verified
  before extraction).
- Extracted once to: `/home/pavel/Laptop_DeepSeek_Max-hold/step034-inputs/contracts-0503391`.
- This directory holds exact byte copies of the archive files used to pin the perpetual
  deadline contract. No fixture bytes were derived or invented.

## Gap: the archive has no standalone `/me` fixture

The archive contains only `schemas/*.json`, `mapping/COMPATIBILITY_NOTES.md` and
`tests/test_perpetual_deadline.py`. It has no standalone `/me` response fixtures and no
projection vectors, so no `/me` JSON bytes could be copied. Instead the exact schema/test
bytes below were copied for provenance. The Go acceptance test reuses the existing observed
`indefiniteMeFixture` (STEP03.5 D2) and cross-checks the pinned 0503391 schema rule and the
pinned Python case matrix.

## Copied bytes (exact)

| Copied path | Archive path | sha256 |
| --- | --- | --- |
| schemas/subscription.json | schemas/subscription.json | `d8bcdae1bfda03b7cffb1b00ef4733d9ee9c5ea90d7c2dc9a43ac29f419e666d` |
| schemas/onboarding.json | schemas/onboarding.json | `81ff8e130c8bdad922fa6464bbc4f495e3ffbaf306dc007810852008a0027814` |
| schemas/common.json | schemas/common.json | `070736d495dd8d0ddc7aa3d4b75a95455a7a5cd1aac151ff8d578bf842696dfc` |
| tests/test_perpetual_deadline.py | tests/test_perpetual_deadline.py | `6b9742b77899021ec5b792d3cd104407001e94f33ec64558ce6fea85a1ec672a` |
| mapping/COMPATIBILITY_NOTES.md | mapping/COMPATIBILITY_NOTES.md | `fefda2e0d2276d00e49a5125e03f453110d26889343f8ff55326c9b7ad10929c` |

Only the `MeResponse` dependency-closed schema set was copied (`subscription.json` ->
`onboarding.json`, `common.json`); the archive's other schemas are not part of this pinned
set.

Pins are recorded in `go_client/accountaccess/accepted_pair_test.go`
(`acceptedFixtureSHA256`).
