# Server payment products and callback ownership — 2026-10-02

Source acceptance; runtime application and real payments remain pending.
Base canonical commit: 69e4dd355674c4061d9e8a04eacbe6fdd7704fd4.
Publication: pavelvital2/terlimo, fix/catalog-stage-deadlines-20261002.

Combines accepted addon expiry-v3 + nearest-rubles-v6 and reviewed callback routing donor 2d4b10afb01f58020ac60f98930d304f1ca483f1. Manifest lists exact13 source pre/post hashes. Migration0036 preserves existing rows, adds paid extra slots/product snapshots and numeric payment amounts. Ordinary addon price uses complete remaining24-hour days and rounds once to nearest RUB, half up;10d5h33RUB,20d67RUB. Subscription/slot end dates unchanged. Unpayable zero-day offers are omitted; new zero quotes fail409 before insertion. Existing frozen quotes/orders retain exact original minor amounts. Selected extras renew independently; expiry eligibility retains earliest bindings and preserves base2.

Routing remains default OFF. Exact opaque provider ID is checked against both existing owner stores before one finalizer; unknown/conflict/lookup failure retryable. Legacy recurring goes to old core. Existing merchant authentication and finalizers retained. Full bounded HTTP ownership response is read until EOF. Old source gate+ownership candidate e342f0b3586f7e4e3eddae660cc4f2e07ae8ae09 is archived as patches only, NOT applied to production. Preserve old callbacks/reconciliation and existing VPN. After new invoices, rollback must preserve owner-aware routing and both cores.

Evidence: original expiry/DTO/core acceptance retained in project history;13 changed-formula/HTTP checks and5 routing-correction checks read, not rerun by root. Canonical pre/post hash equality and combined diff check PASS. No real provider payment, device flow or runtime acceptance inferred.

Compatible built client: source69e4dd3, APK2a1150d30db60e68a3a5bc24cd0229e0645631199990d55038366843c2919338; packaged native0634c321903ac5a3676e4429023b9873cd3dc6b8dea91dd61994d49e55470344. No APK rebuild for server pricing. Prior client history: ../client-payment-products-20261002/README.VERSION.md; prior working pair ../working-pair-20261002/README.md. Those earlier receipts remain historical and unchanged.

TEST application uses scoped13-file backup, consistent private DB dump, existing migrate runner and API/worker restart only, provider/routing OFF. Preserve other dirty overlays/env/node/relay. Before financial writes guarded0036down+scoped source restore possible; after quotes/orders retain ledger/schema and forward repair, no stale dump restore. Production cutover remains a separate reviewed package.
