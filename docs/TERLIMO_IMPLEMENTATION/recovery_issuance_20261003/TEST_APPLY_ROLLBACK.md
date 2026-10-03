# Recovery issuance: SOURCE package, later TEST only after root acceptance

## Exact entry points for Android buttons

| Channel | Official production target (prepared, NOT published) | Actual existing TEST target (new behavior pending apply) |
| --- | --- | --- |
| Bot | https://t.me/terlimo_vpn_wdtt_bot?start=recovery | https://t.me/terlimo_test_bot?start=recovery |
| Site | https://terlimo.xyz/api/public/recovery | https://step036.193-5-251-217.sslip.io/api/public/recovery |

These links contain only a fixed action, no installation/account/session/order values. Production bot is the existing Minishop bot, not the TEST poller. Production site uses the existing Minishop backend GET route; `/api/` already proxies through its unchanged frontend nginx configuration. No new frontend, proxy/Caddy config or service is required in this SOURCE package. Site is anonymous and works without registration/purchase; GET code has no auth/DB call. A page is not a guarantee of site reachability in a restricted operator network.

## Source provenance

Canonical pavelvital2/terlimo exact base2d823640c3823a678bee5e3d4497925d4ec57f64 in isolated `source/`. It contains the actual current TEST registration bot and backend, but no production Minishop frontend/bot. Additional isolated `minishop-source/` is cloned from the existing verified clean `/home/pavel/projects/terlimo-minishop` exact base1bf361f604683cff31ed2a84b2163393761a9742, upstream3252a8/remnawave-minishop. Its bot middleware/menu and existing webapp route seams are changed. This proves SOURCE provenance, not current production image parity. Root retains both patches in canonical and is sole publisher.

Verifier/reader in Minishop is derived from canonical `server/terlimo_backend/recovery_code.py`: only the application Settings import becomes a three-property Protocol; formatting differs, signed format/validation/cache behavior and cross-language fixture are unchanged. Same signed file/verifier drives both channels. Web page renderer shares canonical behavior, with existing ru/en locale keys supplied in Minishop. No operator key is loaded by either channel.

## Latest code and migration

Current TEST `RECOVERY_CODE_FILE=/home/pavel/step036-device-stage/pkg/secrets/recovery-code.v1.txt` still serves signed revision2 (SHA receipt in latest-code-source.safe.json); current phone accepted revision4. Do NOT issue2 or the alternate3 pointing to the removed TEST2 forwarder. Root's accepted primary4 code (674characters, canonical file675bytes includingLF SHA f570ecf38df6c8bf538e1cffd1afd60630eecb626da92e3d079c24ef60b205b7) and public verifier are the required TEST input. Only primary2 bytes were delivered to engineer by the original root envelope; primary4 bytes remain a root handoff input. No fixture or unsigned seed is a deployable substitute.

Run the supplied preparation helper with root's existing primary4/public verifier paths; it verifies exact accepted hash/key/signature/environment/revision/primary endpoint and stages files0600 under0700, without live writes. Inputs contain no private key. The staging helper and SOURCE payload are ready; actual staging awaits those bytes.

Existing offline `recovery_sign` is the sole producer from the latest approved complete Seed (endpoint/DTLS pin/classifier/VK set/stream/environment/newer revision). Use the existing operator-held signing key and existing client public trust; never copy private key to bot/site or generate another key/service. New `recovery_publish` installs an ALREADY signed code atomically, after approved minimum revision, prior SHA, environment and signature checks. Same revision/different bytes and older revisions are refused. Readers detect inode/metadata rotation on the next action/GET; corrupt/missing file returns unavailable, never a stale cached code. It does not call an old backend or add a mandatory client resolver.

For a later production move, distribute the newly signed public file to the existing bot/site deployment at its current stable URLs, including its new endpoint/VK set. Issuer config points to the local current file, not an HTTP fetch from the decommissioned backend. Deploy/migrate bot/site themselves through root's separate production gate. Production uses `TERLIMO_RECOVERY_ENVIRONMENT=production` (default in Minishop), its production code and public verifier; NEVER publish a TEST code there. Standalone canonical TEST uses existing Settings.environment=test. Both readers enforce environment separation.

## Proposed bounded TEST application (NOT executed)

1. Root accepts exact heads/patches and supplies primary4/public verifier. Run `prepare_test_package.py --accepted-primary4-code PATH --public-verifier PATH`. Verify package SHA manifest. Do not sign/rebuild/repeat phone recovery suites.
2. Fresh readback existing5 source paths in `/home/pavel/terlimo-test-a-live/terlimo_backend/`, original env and signed public file, API/bot PID/command identity, dependency graph. Compare against `test-source-preimage.safe.json` and current accepted state. Existing3 affected files exactly matched canonical base at SOURCE preparation; page/publish modules were absent. If changed, resolve narrow difference before install. No secret values enter report.
3. Private root0700 backup indexed source existence/bytes/mode/uid/gid, API env, bot launch/env and original public file0600. Do not back up/restore DB or client state as part of this patch.
4. Atomic install only changed recovery_code/recovery_api/recovery_page/telegram_bot plus operator recovery_publish into existing source tree, preserve ownership. Existing API registration already calls recovery route registrar. Existing API/bot public env fields stay the same.
5. Run SOURCE publisher against accepted primary4 code/public verifier and the existing `RECOVERY_CODE_FILE`, environment=test, minimum_revision=4, exact fresh current-file SHA, uid/gid of the existing pavel process (1000/1000 here), destination0600. Bot/API must read the file; parent traversal retained. No signing key on service host is required for this rotation. An operator always records the new highest floor.
6. Under separate root-approved TEST apply only, restart existing API and ONE existing bot poller using its saved existing launch/env (never create a second poller). Check relay/node Requires graph first; do not restart relay/node/worker/PG/edge or change Caddy. This plan does not itself grant these runtime actions. Server source-only work performs none.
7. Existing HTTPS GET public page exact200/no-store/noLocation and primary4 code hash; with normal browser explicit Copy success plus clipboard-denied fallback/selectable code. Existing bot /start recovery and menu action issue the same primary4 block/instruction without DB/rights/registration writes. Check no data rights/account/payment changes, no signing key read, diagnostics remainOFF. No primary2/alternate3/return4 transport replay, reboot/updater/full suites.
8. Minishop SOURCE package remains for root's later production preparation/acceptance. No live production action or existing OLD bot modification was done here. Existing TEST bot/page verifies issuance behavior; it does not prove production reachability or publication.

## Rollback

SOURCE: discard isolated checkout/payload only; original workspaces are unchanged. Later TEST: stop only the one existing bot poller/API in the same authorized scope, atomically restore indexed source/env preimage (remove only newly absent modules), restore saved same-process launch and API. Retain the valid accepted primary4 public file: never knowingly reissue2 after client floor4. If code publication itself failed before atomic replace, original file remains unchanged and issuance should stay unavailable until corrected. If a newly published code is incorrect after clients accepted it, produce a corrected NEWER signed revision with the same trusted key; do not force a client revision rollback or restore old DB. Source rollback may remove the newly exposed public route; root keeps it unavailable rather than claiming an older code works. Restore original modes/owners, check protected services, no full firewall/Caddy/DB restore.

## Tests and boundaries

Existing recovery format/signature/no-extra-fields/auth/service-seed/registration regression tests passed54 cases. Four additional canonical tests cover anonymous page, file rotation/new tuple, deep link bypass, actual shipped clipboard JS success/denial/absence and atomic publisher guards/ownership. Four Minishop tests execute public middleware, callback, independent local file, production/test separation, and real aiogram Dispatcher with unavailable account FSM. Only SOURCE/offline results. Current TEST/production route/bot behavior has NOT been changed or live accepted. Full Minishop repository suites/build/mypy were not run; no push/publication in this task. Affected lint and scoped types are recorded separately.
