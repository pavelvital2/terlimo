# Public recovery issuance — SOURCE only

Canonical existing API/bot now support fixed /start recovery and anonymous GET /api/public/recovery. Page includes readonly code, explicit Copy, clipboard-denied manual selection, no-store and public-file signature/environment verification. Code carries only the accepted transport Seed, no identity/grant. Authenticated service-seed stays authenticated. Existing producer recovery_sign is unchanged; recovery_publish atomically installs already signed code after minimum revision/prior SHA/environment checks with0600 ownership.

Root package includes the additional existing Minishop source patch at exact base1bf361f604683cff31ed2a84b2163393761a9742: public recovery action before account FSM/DB, two existing menus, same verifier/reader and anonymous webapp route withru/en text. Production entry URLs: https://t.me/terlimo_vpn_wdtt_bot?start=recovery and https://terlimo.xyz/api/public/recovery. TEST: https://t.me/terlimo_test_bot?start=recovery and https://step036.193-5-251-217.sslip.io/api/public/recovery. These URLs are prepared, not live issuance PASS.

Migration uses new approved complete transport tuple/VK set signed offline by the existing trusted operator key, atomically rotated in local public files at existing bot/site deployments. Readers never fetch from the old backend. Deployment of current issuer host/URL remains separately authorized. No new protocol/key service/client resolver.

Current TEST public file is stillrevision2; accepted client floor4 requires root accepted primary4 file before TEST apply. Do not publish TEST code on production. No runtime/Caddy/network/production/phone action in this SOURCE task. Full apply/rollback package is in the engineer handoff, root sole publisher.
