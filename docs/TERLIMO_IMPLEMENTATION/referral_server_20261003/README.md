# Referral server implementation package

Canonical base: ba3934f683616cdc4572d3fe8e81f9d0ed8e63b7. Latest doc-only reference: 7c4879409a4904d40e06cb69debc62287c61e43b. Contract27 and accepted native/registration fixtures govern this SOURCE.

Implemented on the existing backend: migration0038, protected history/reward import, permanent account codes and attribution, durable keyed candidate operations and immutable registration receipts, trial10 and readback-driven inviter rewards, MAIN10000minor discount with account reservation and immutable pricing, first-paid anchor, late canceled CONFIRMED reconciliation and committed exact-quote no_order invalidation. Existing auth, payment core, grant/outbox and worker remain authoritative.

Read CHECKS.md for actual results and baseline fixture failures; LOCK_ORDER.md for concrete writer order; TEST_APPLY_ROLLBACK.md for subsequent compatible TEST application; INTEGRATION_REMAINING.md for bot/site cutover seams. Protected staging and additive payment wire fixtures are in ../referral_20261003/. Root reviews and publishes the exact committed patch; this package performs no runtime application.

Root review corrections continue from accepted fixture commit dc8262877ebc20507442e9777a02a67386b26a05. CORRECTION_CHECKS.md supersedes the prior monotonic-target/fairness/paid-lock gap with targeted evidence; preserved fixtures are part of the cumulative source. Runtime and release integration remain separate.
