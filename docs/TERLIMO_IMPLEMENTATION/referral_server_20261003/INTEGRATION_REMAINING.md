# Existing UI seams and single writer cutover remainder

Canonical accounts.telegram_id is the verified mapping key. Client supplied account_ref/TelegramID/code is not ownership proof. The new mobile account backend is not an already deployed equal referral program across all three UIs.

Read-only seam inventory from retained Minishop SOURCE1bf361f604683cff31ed2a84b2163393761a9742 (not a claim of current production image parity):

- Bot: backend/bot/handlers/user/start.py saves the received ref on verified registration; backend/bot/handlers/user/referral.py referral_command_handler/referral_action_handler and _generate_webapp_referral_link display the existing stored code and links.
- Site: backend/bot/app/web/webapp/auth_referral.py _resolve_referrer_id, _apply_referral_to_existing_user, _ensure_user_from_telegram operate on verified Telegram identity and first attribution; referral_links.py visible_referral_links is the existing link display seam.
- Existing business writer: backend/bot/services/referral_service.py apply_referral_bonuses_for_payment / generate_referral_link and its subscription extension dependency; deployed awarding/source rules are preserved in ../referral_20261003/REFERRAL_RULES.md. Existing award intents/history must be imported, not replayed as newly earned events.

Future separate production cutover:

1. Root pin current actual image/code/schema and designate exactly one canonical referral writer. Freeze only competing referral award/create operations for a bounded cutover while preserving payment callbacks, normal access and unrelated work.
2. Protected import captures legacy verified Telegram→account mapping, stored spelling/code uniqueness, earliest attribution and actual trial/firstmain-paid/earned/applied history. Reconcile into existing accounts; conflicts remain review/history_pending, no competing random code. Record snapshot watermark and a delta catchup before enabling the writer. Source fixtures exercise importer; this task reads/imports no live production data.
3. Route the bot/site seams above to the same account writer/immutable receipts or explicitly migrate those writers and disable their competing legacy award path. Reuse existing authenticated server mechanisms, never a new public identity envelope. Both interfaces show accountcode regardless of expired subscription and same approved terms. Existing email/login paths are preserved; verified mapping policy must be explicit for legacy accounts lacking Telegram ownership proof.
4. End-to-end acceptance across nativeapp, actualbot and site checks sameaccount/code/firstattribution, onceonly trial/paid awards, immutable100RUB payable price and waitingday state. Root then publishes, not the engineer in this SOURCE stage.

Official links from accepted nativefixtures: https://t.me/terlimo_vpn_wdtt_bot?start=ref_uCODE and https://terlimo.xyz/?ref=uCODE. Code entry is the approved first-release flow. No deferred attribution through APK installation is promised.

Cancellation remainder: local canceled/expiry/404/UIclose is not provider final-nonpayable proof. Original reserve remains reconciling and original ID is rechecked through the existing bounded core; matchingpaid consumes once. Automatic release awaits a proved authoritative provider path, with no owner/support action required for independent SOURCE work. No coupon/provider discount API, real invoice or refund is involved here.
