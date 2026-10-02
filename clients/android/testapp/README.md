# TERLIMO TEST Android host

Isolated application ID: `xyz.terlimo.test`. Android API 28+, target 35, compile
36, arm64-v8a; uses pinned upstream WireGuard tunnel 1.0.20260102. No upstream
admin UI, server assets, deployment controls, or legacy profile imports.

## Provisioning and build

`src/main/assets/issuers.json` contains exactly the authorized public TEST issuer
delivered on 2026-09-10. The immutable source bundle is `test-public-trust.json`,
SHA256 d3a54153dbc936b503dcab55da34a159787069272ce23beb59e86612c9e4e94f.
There is no self-signed fallback or import-time trust prompt. No synthetic
fixture issuer is trusted. Native accepts only signed links with env=test.

`src/main/assets/test-probe.json` must contain the authorized public HTTPS
`probe_url` and exact `expected_exit_ip`. The endpoint must return status 200
and the plain expected IP, at most 256 response bytes. Redirects fail closed.
Both fields now match the authorized public bundle; missing provisioning
still prevents Connected. The bundle's node/pin/cap fields document the expected
TEST deployment: runtime uses the verified link/catalog values and checks DTLS
SPKI against that authenticated pin. The server's loopback WG port is never used
as a direct phone endpoint. No subscription/access secret is an APK asset.

The native Go subprocess belongs at
`src/main/jniLibs/arm64-v8a/libterlimo.so`, built as an Android PIE executable.
Its basename is a packaging convention, not a JNI library. Launch uses
`nativeLibraryDir/libterlimo.so --android-bridge`, no secrets in argv or env.
Native build and APK assembly are coordinated by the root task.

Source checks: `android-env gradle :testapp:compileDebugKotlin
:testapp:compileDebugJavaWithJavac :testapp:testDebugUnitTest`.
Do not run connected Android tests or install/launch without separate approval.

## Host boundary

- Anonymous child stdin/stdout only, versioned JSONL, 256 KiB maximum line.
  Every message carries the active attempt ID; stderr is discarded.
- P-256 key generated once in Android Keystore, no export/backup, no required
  StrongBox/biometric prompt. Signer receives exact domain-separated T in
  SHA256withECDSA and returns DER. It does not receive or rehash a digest.
- AES-256-GCM Keystore storage in noBackupFilesDir; atomic writes are ACKed
  before native mutation proceeds. Cancel retains key, link, registration and
  pending operation. Key loss is terminal, never silent replacement.
- Native receives a non-VPN physical network handle and must bind its own
  process before any network operation. Physical network loss cancels the
  attempt. Host is not excluded from VPN.
- SafeGoBackend is a scoped copy from pinned upstream with physical underlying
  network and fail-closed socket protection. Only loopback native relay
  endpoints are accepted. No raw public WireGuard endpoint fallback.
- Connected requires WireGuard handshake, DNS through the VPN Network, and
  a VPN Network-bound HTTPS response matching the provisioned expected IP.
  Authentication OK and interface creation alone never imply Connected.
  The handshake timestamp must be at least this apply attempt's start time,
  and the peer must have both RX and TX bytes. TEST accepts IPv4 interface
  addresses, DNS and AllowedIPs only; IPv6 in any field is rejected.
- Confirmed server/wall-clock lease budget is capped at 15 minutes, translated
  to elapsedRealtime. Same-expiry events cannot extend its local deadline.
  A lease-bounded partial wake lock prevents uptime-based timers extending
  through CPU sleep; it never turns the screen on and is released on teardown.
- CAPTCHA uses a bounded, user-opened private WebView with v17 success-token
  interception, HTTPS VK-origin restrictions, no file/content/mixed access,
  no token logs, and correlated timeout/cancel. Runtime WebView compatibility,
  challenge asset origins, and CAPTCHA during active refresh remain NOT_TESTED.

## Verification limits

Unit tests check shared synthetic signature vectors (including negative
double-hash), queue limit, cancellation/old attempts, transcript type/size and
base64 canonicality. Additional tests check fresh-handshake/RX/TX readiness,
IPv6 rejection, typed worker metadata and cancellation-safe state publication.
Instrumentation tests cover Keystore signature verification
and encrypted storage round-trip but require an authorized device run.

No phone install, ADB, VPN launch, live VK/TEST call, server change or physical
device PASS is implied by compilation or host tests. Live issuer/pin agreement,
native execution, foreground lifecycle, DNS/WireGuard/data flow, expiry/revoke
under suspend, and system-VPN restoration require explicit TEST acceptance.

## Payment plans: optional session authentication

Native `accountaccess.Client.ListPlans` uses the existing service TokenSource and
HTTP/service-channel route for `GET /plans`. If configured, that source supplies the
Bearer so plans reflect the same account pricing as quotes. Token acquisition or
validation errors propagate; there is no retry with anonymous prices. With no
TokenSource the public request remains allowed, without Authorization. No token or
account ID comes from the UI; no new login flow is introduced. Existing session
refresh policy, request deadline, endpoint and response/bridge schemas are unchanged.
