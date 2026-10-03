# Referral client source acceptance — 2026-10-03

Root accepted cumulative client changes from Laptop 291216bb → c18d16b → c4608b, against published native registration contract ba3934f. Thirty Android/native source and test files integrate account referral info, durable candidate operations, keyed Telegram registration and correlated terminal receipts, plus TEST/production recovery issuance links. No accountaccess dependency patch is applied twice.

The installation journal persists original operation and registration keys before sending, replays unchanged requests after unknown results, and accepts terminal registration only against installation/candidate/key/registration/fresh-account fences. Proven expired registration preserves the candidate and requires an explicit new user action for a new key. Old journal records lacking correlation remain locked. Lifecycle refresh neither creates a registration intent nor reopens Telegram.

Root reviewed native bridge, strict API delegation, fresh account handling and issuance URL boundaries. Independent bounded source review accepted the host registration journal/receipt integration. R1 correction now requires nonretryable native rejection and explicit JSON Boolean false in the host proof; ambiguous errors preserve the original key/body across restart. Four R1 postimage hashes and all cumulative publisher postimages match the reviewed source.

Executor evidence: completion Android compile and 68 affected JVM tests passed; 10 named Go tests passed. R1 Android compile, 8 gate tests and one Go test with 10 cases passed. Root checked source/hash/diff evidence without repeating accepted suites.

This is SOURCE acceptance, not end-to-end referral acceptance. Backend endpoints, benefits/payment/reward integration and a compatible TEST build/run remain required. The installed version16 is unchanged. Recovery website copy is accepted separately; Telegram user check initially could not connect without VPN. Owner confirmed that prerequisite; the existing Laptop session received a bounded continuation using existing TEST access. Production deployment has not occurred.
