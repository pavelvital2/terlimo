# Recovery v1 server source

The existing product bot and authenticated service update read the same signed
public file. Neither loads an operator signing key. No migration, subscription,
account change or background update job is involved.

## Operator inputs (deployment is a separate step)

- `RECOVERY_CODE_FILE`: path to an ASCII public TR1 code (optional trailing newline).
- `RECOVERY_VERIFY_KEY_B64`: trusted deployment Ed25519 public key, canonical
  base64url without padding, exactly 32 decoded bytes. Not a key supplied by the code.
- `TERLIMO_ENV`: `test` or `production`; the signed seed must match it.
- Bot still uses `TELEGRAM_REGISTRATION_BOT_TOKEN`. Registration retains its
  existing `TELEGRAM_REGISTRATION_BOT_USERNAME` and `TELEGRAM_REGISTRATION_BOT_KEY`
  checks. Public recovery works without a registration binding or application DB.

Generate offline, with an **explicit** operator-owned Ed25519 PKCS8 PEM key path:

```sh
python -m terlimo_backend.recovery_sign --seed-file public-seed.json \
  --private-key-file /operator/private/ed25519.pem --environment test \
  --output public-recovery-code.txt --public-key-output public-verifier.txt
```

Private key input is not written to settings, bot, logs or output. Seed fields are
`version`, `revision`, `environment`, `peer_ip`, `dtls_port`,
`dtls_spki_sha256`, `service_classifier`, `vk_hashes`, `stream_id`.
The producer requires explicit revision and environment and emits fixed field
order, compact UTF8 JSON. The accepted Android arm64 build's signed Go integer stream ID bounds are retained.
The tool does not issue or configure a deployment key.

Code: `TR1.<raw-url-b64 UTF8 seed>.<raw-url-b64 Ed25519 signature>`.
Signed bytes: `b"TERLIMO-RECOVERY-V1\x00" + exact decoded seed bytes`.
Verification does not reserialize JSON. Duplicate/unknown fields, wrong type,
foreign environment/signer, padding, inner whitespace, malformed/truncated input
and codes over 3500 trimmed ASCII characters are rejected. Public file bound is
3502 bytes (code plus CRLF); seed bound 8192 bytes, 1–4 VK hashes of at most
128 UTF8 bytes. Existing public classifier/pin/port/IP bounds are retained.

One validated code is cached per API/bot instance. File device/inode/size/mtime/
ctime changes trigger bounded reread/verification. Missing or invalid replacement
returns unavailable, never a fabricated code. No TTL, scheduler or DB producer.
Update by replacing the public file; environment/verifier changes require the
separately authorized process configuration update.

## Exact API wire

`GET /api/mobile/v1/service-seed`, normal `Authorization: Bearer …`.
Existing `authenticate_session` validates identity/revocation/environment/binding;
there is no data-grant, subscription, Telegram eligibility or special scope check.
The existing mobile response conventions are used:

```json
{
  "request_id": "<server-generated mobile request id>",
  "server_time": "<RFC3339 UTC>",
  "schema_version": 1,
  "status": "ok",
  "recovery_code": "TR1.…"
}
```

The existing HTTP middleware independently echoes `X-Request-ID`. The service
transport envelope retains its original request pairing. No field is added to
`/me` or to the strict Seed JSON; the signed code is the only new success field.
Errors use the existing envelope `request_id/server_time/schema_version/status`
plus `code/retryable`: missing/invalid public config -> HTTP503
`RECOVERY_UNAVAILABLE`, DB unavailable -> HTTP503 `TEMPORARILY_UNAVAILABLE`,
existing auth errors keep their HTTP401/403. Only exact GET is allowed by the
node and Python guards; the client's Go guard is integrated by the client owner.

## Bot

`/start` and registration replies present the existing-bot reply keyboard button
**Восстановить подключение**. The button or `/recovery` (also addressed to this
bot username) sends the entire TR1 code as **one plain text message**, no markup,
prefix or extra instructions. Missing/invalid configuration gives an explicit
unavailable message. Public handling precedes any application DB acquisition;
bot startup no longer requires a DB connection. Registration lazily reconnects
the same configured DSN and retains the existing confirmation handler.

## SOURCE proof / remaining acceptance

`tests/fixtures/recovery-v1-test-vector.json` is a shared deterministic Python→Go
vector and exact HTTP example with **public TEST dummy key bytes 00..1f**, TEST
classifier/hash, documentation IP `192.0.2.123`. Never deploy that fixture.
Tests use fake Telegram IO and isolated HTTP handlers; no actual Telegram calls.
Client candidate lifecycle, monotonic saved revision, cancellation, atomic
persistence, restart and four targeted phone cases belong to the client/root
integration stage. Server issuance is not evidence of a recovered connection.
