# WDTT direct v3 adapter — bounded SOURCE acceptance

Accepted donor e9c0689d8756ac9b971b3d0b3ada7dc0793152ba over published93e5930f6b225757d8a5e32954bbcd5103574b29. Root verified all282postimage hashes, artifact hashes, exact full/delta source and affected tests/logs. Original25offline cases retained. A1 adds9 actual-parser failure cases and reruns1 ACK recovery case; J1 adds3 temporary SQLite restart cases. Root did not repeat executor suites.

A1: only normal admin completion persists acked; malformed/noACK exceptions keep issued even if generic uncertain=False. J1: exact natural BASE expiry before any issued command allows original saved intent to finish through fixed-expiry update/activate/readback. Foreign expiry and issued without ACK remain blocked. Independent bounded journal review found J1; root confirmed and reviewed correction. Source acceptance is limited to the adapter, durable journal, wire and offline proof.

No live deployment/enablement, canonical business delivery hooks, production writer transfer or end-to-end VPN acceptance. Default v3 OFF. Unacknowledged issued mutation remains pending; absent→inactive cannot be enacted by available create command. These are explicit unresolved protocol limitations, never manufactured applied receipts. External privileged writers require separate ownership transfer. Adapter artifact remains protected Unix response only, not journal/log/evidence.

Next: strict canonical adapter client and account delivery integration using immutable target/source/revision receipts, then preserved bot/site inline callers and controlled TEST acceptance. Existing public mobile/reward behavior not changed by this commit.
