# Recovery and Android updater acceptance — 2026-10-03

## Owner boundary

The owner requested completion of the current recovery verification and real in-app update, followed by a checkpoint and stop. These accepted scenarios are complete. Laptop and TERLIMO have saved checkpoints; the project controller is paused. Remaining features require the owner's discussion and decisions before implementation. This is not acceptance of the whole production release.

## Exact accepted artifacts

Both updater APKs derive from source `1bd98e4a8de741866162ae17f2c274b78835529b`, Android tree `7f8163abb8cb6350a311294596723b9d6e9cb8a9`, with separate version-only overlays attached here. Later canonical changes are not silently included in these tested binaries.

| Artifact | SHA-256 | Bytes |
| --- | --- | ---: |
| V15, 0.15-update-test | `39b2f3afcad283c9073a8f5cffe706712a8385d0e4f2338cd6016f40b332e138` | 18434705 |
| V16, 0.16-update-test | `3d25215f63d03ba7b0142acb01134cdf2882f48f797ca7b483e98c0be8571e39` | 18434701 |

Package `xyz.terlimo.test`; signer certificate SHA-256 `42ab6d950c15742eaadcf538947f65b3e0bd3e7e1693d67afa969eb00e5d348b`; minSDK28, arm64-v8a. Normal TEST configuration SHA-256 `73a13d11d96f08daf137a5311b674f48d67b981bfbd7635059c76d293366e57d`. Packaged native SHA-256 `e21cd6f4c4976de306f40185ade0d24e12cd025ded144fe9be9564ad4cd622e4`.

These are TEST artifacts and signing identity, not a decision about production package identity or release signing. Actual Android compilation, existing affected JVM checks and artifact/signature checks were accepted previously; they were not repeated for runtime acceptance.

## Recovery runtime accepted

On the existing TECNO test installation, the remaining sequence completed: fresh service connection after reboot using the saved alternate endpoint, one explicit return-to-primary revision4 code application, visible saved-success message, then one cold app restart and a fresh authenticated account/catalogue from the primary endpoint. Identity and existing account, binding, rights and financial records were preserved. Ordinary grant lease advancement was recorded separately rather than described as unchanged grant bytes.

Client window: 14:18:40.265851–14:27:21.089923 UTC. Server AUTH/session/ME and direct-versus-alternate peer evidence correlate with the three client stages. Device clock samples were approximately one second behind host UTC; no precise per-hop timing is inferred.

The earlier Unix relay timeout did not recur. Instrumentation observed a 4.10-second asynchronous challenge-insert wait and a 1.45-second signature-verification interval in the successful run. These do not establish the cause of the historical timeout. Restart is not claimed as a proven fix; no speculative source or timeout change was made.

Original diagnostic flags and environment file hashes were restored after the whole phone terminal. API/relay and the explicitly authorized dependent-node restart completed with healthy listeners. Other protected services were preserved. The four temporary TEST2 forwarding rules were later removed by exact specification, with all unrelated rules preserved and a private targeted rollback saved. No production changes were made in this sequence.

This acceptance covers the stated remaining recovery runtime sequence; it does not invent a new tampered-code runtime test or claim every future recovery/distribution scenario is complete. Telegram/site delivery of recovery codes remains a separate product item.

## Actual updater acceptance

Runtime window: 14:51:46.374706–15:04:09.705704 UTC, on the same test phone.

- A successful user service connection on V15 automatically displayed the V16 offer before any manual update check.
- Later deferred the offer; an explicit Settings check offered it again.
- The application's own downloader fetched and verified the exact V16 APK. No ADB installation substituted for the update.
- Android's per-app installation permission was granted through system UI. Returning required explicit install consent again.
- Cancelling the Android installer retained V15 and unchanged durable data; explicit retry proceeded through the real installer.
- Google Play Protect requested and completed its normal scan. Protection was not disabled or bypassed. The user-visible Android installation completed.
- Installed APK readback matched exact V16 hash, size, version and original signer. All nine durable files were byte-identical across package replacement, including installation marker and encrypted account/settings/payment/recovery state.
- Opening V16 obtained fresh authenticated account/catalogue and displayed retained active access. Subsequent normal refresh changed encrypted runtime state and updater/profile bookkeeping; equality of the entire post-launch store is not claimed.
- Final service/native/VPN state was OFF. No registration, trial activation, payment, reboot or VPN suite was repeated. The per-app Android install permission remains granted after the tested flow.

Root reviewed safe result metadata, bounded runtime logs, and screenshots of the automatic offer, Android installation success and retained access. This proves the TEST V15-to-V16 user flow, not silent installation or general production readiness.

## Distribution checkpoint

TEST static hosting serves the accepted V16 manifest (SHA-256 `a4662aea6a8b96e45555d1bee3bec18b6e3c192fdfcdeb18c2dc7165149ea665`, 448 bytes). Normal TLS GETs returned 200 without redirects; manifest/APK readback matched exact bytes and hashes. Publishing placed the immutable APK first and switched the manifest atomically; existing V15 and V16 artifacts are retained. No Caddy reload or service restart was needed for the metadata switch.

Rollback is limited to restoring the accepted V15 manifest atomically; it does not downgrade an already installed V16 or revoke an already downloaded signed APK. No uninstall, data clearing or blind server/DB rollback is authorized by this checkpoint.

## Evidence and next boundary

Safe evidence is archived in the project management workspace under `docs/management/S1_20260919/CATALOG_STAGES_RECOVERY_20261002/`:

- `recovery-unix-diag-client-terminal` and `recovery-unix-auth-final`: correlated remaining recovery runtime and diagnostic restoration.
- `recovery-v15-installed-ready`: initial V15 preparation and preserved data.
- `app-update-v16-static-ready`: targeted cleanup and exact static readback.
- `updater-ui-final`: actual installer flow, final APK and data verification.

Accepted result events: `terlimo-20261003-recovery-unix-auth-final-v1`, `terlimo-20261003-app-update-v16-static-ready-v1`, `laptop-20261003-updater-ui-final-root-v1`. Root retains original verified manifests/hashes locally; full private logs, environment data and device state are not published here.

Always-on has an accepted source checkpoint but no AT22 device acceptance. Referral rules and the five-tab function mapping remain discussion inputs. No Always-on runtime, referral implementation or design work is started by this acceptance. Resume only after the owner's next decision, discussing one unresolved item at a time.
