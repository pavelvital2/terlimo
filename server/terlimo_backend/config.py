"""Runtime configuration. Values come from the environment; no secrets in the repo."""

from __future__ import annotations

import os
from dataclasses import dataclass
from urllib.parse import urlsplit, urlunsplit


class ConfigError(RuntimeError):
    """Raised when required configuration is missing or invalid."""


def redacted_database_url(database_url: str) -> str:
    """Return the DSN without credentials for logs and readiness payloads.

    Adapted from the donor helper in MiniShop cb8caf41
    backend/db/database_setup.py (redacted_database_url), SQLAlchemy-free.
    """
    try:
        parts = urlsplit(database_url)
    except ValueError:
        return "<invalid database url>"
    if not parts.scheme:
        # Unix-socket DSN forms without scheme are not logged verbatim.
        return "<database url without scheme>"
    netloc = parts.netloc
    if "@" in netloc:
        netloc = netloc.rsplit("@", 1)[1]
    return urlunsplit((parts.scheme, netloc, parts.path, parts.query, parts.fragment))


def _env_str(name: str, default: str | None = None) -> str | None:
    value = os.environ.get(name)
    if value is None or value == "":
        return default
    return value


def _env_bool(name: str, default: bool = False) -> bool:
    raw = _env_str(name)
    if raw is None:
        return default
    return raw.strip().lower() in ("1", "true", "yes", "on")


def _env_int(name: str, default: int, minimum: int) -> int:
    raw = _env_str(name)
    if raw is None:
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise ConfigError(f"{name} must be an integer") from exc
    if value < minimum:
        raise ConfigError(f"{name} must be >= {minimum}")
    return value


def _env_float(name: str, default: float, minimum: float) -> float:
    raw = _env_str(name)
    if raw is None:
        return default
    try:
        value = float(raw)
    except ValueError as exc:
        raise ConfigError(f"{name} must be a number") from exc
    if value < minimum:
        raise ConfigError(f"{name} must be >= {minimum}")
    return value


@dataclass(frozen=True)
class Settings:
    database_url: str
    environment: str
    log_level: str
    api_host: str
    api_port: int
    db_pool_min: int
    db_pool_max: int
    db_command_timeout_seconds: int
    worker_poll_interval_seconds: float
    worker_lock_timeout_seconds: int
    worker_max_attempts: int
    # Defaulted last so existing constructions (review reproducers, tools) stay valid.
    db_reconnect_cooldown_seconds: float = 1.0
    # PoP/installation/session profile values. The accepted contract leaves challenge TTL,
    # session TTL and the timestamp skew as pending profile decisions (S1-B02); these are the
    # provisional bounded TEST values for step 03.2 and are configurable.
    challenge_ttl_seconds: int = 300
    session_ttl_seconds: int = 86400
    proof_skew_seconds: int = 300
    challenge_rate_limit_per_minute: int = 20
    # Public (unauthenticated) issuance cap on the shared PostgreSQL. The accepted contract
    # fixes no number; this is a documented engineering TEST profile.
    public_challenge_limit_per_minute: int = 20
    # Retention/cleanup (provisional TEST profile, see README).
    challenge_retention_seconds: int = 86400
    receipt_result_ttl_seconds: int = 3600
    idempotency_window_seconds: int = 604800
    cleanup_interval_seconds: int = 300
    cleanup_batch_size: int = 500
    # Entitlement pre-expiry reminder sweep (runs inside the maintenance loop).
    reminders_enabled: bool = True
    reminder_window_seconds: int = 259200
    # Outbound Telegram reminder notification via the single official product bot. Fail-closed:
    # without an explicit owner enable and a configured bot token nothing is sent.
    telegram_notifications_enabled: bool = False
    # Catalog validity window (provisional TEST profile; finite by contract).
    catalog_validity_seconds: int = 600
    # Gateway control (existing WDTT admin wire). The main password is an environment
    # secret for the local admin socket; it is never stored in the Backend database.
    gateway_admin_main_password: str = ""
    gateway_admin_timeout_seconds: int = 10
    # Protected runtime secret for onboarding bootstrap credentials (Fernet key). Empty means
    # "cipher unavailable" and every onboarding secret operation fails closed.
    onboarding_secret_key: str = ""
    # Backend mTLS client identity for the gateway management handler (§5.2).
    # Explicit isolated-TEST opt-in for the local admin socket transport; never a silent
    # fallback, and never allowed outside environment=test.
    gateway_local_admin_enabled: bool = False
    gateway_management_ca_file: str = ""
    gateway_management_cert_file: str = ""
    gateway_management_key_file: str = ""
    # Finite technical grant ceiling; CONTROL_BOUND_PROFILE stays provisional TEST (B02 STOP).
    gateway_max_lease_seconds: int = 900
    # G6 evidence transport (open seam): dedicated backend mTLS listener + AF_UNIX technical
    # relay. Separate opt-ins per role so each side needs only its own material; disabled
    # roles fail closed without any plaintext/X-header fallback.
    evidence_endpoint_enabled: bool = False
    evidence_server_cert_file: str = ""
    evidence_server_key_file: str = ""
    evidence_client_ca_file: str = ""
    evidence_listen_host: str = "127.0.0.1"
    evidence_listen_port: int = 0
    evidence_relay_enabled: bool = False
    evidence_node_cert_file: str = ""
    evidence_node_key_file: str = ""
    evidence_backend_ca_file: str = ""
    evidence_backend_host: str = ""
    evidence_backend_port: int = 0
    evidence_backend_server_name: str = ""
    evidence_relay_socket: str = ""
    evidence_relay_allowed_uid: int = -1
    evidence_relay_allowed_gid: int = -1
    evidence_timeout_seconds: int = 15
    # G6 service-only API relay (open seam): fixed loopback replay of the existing mobile
    # API for the node service channel. Separate opt-ins; endpoint reuses the listener's
    # server material, relay reuses the node material.
    service_endpoint_enabled: bool = False
    service_relay_enabled: bool = False
    service_relay_socket: str = ""
    service_relay_allowed_uid: int = -1
    service_relay_allowed_gid: int = -1
    service_timeout_seconds: int = 15
    service_upstream_host: str = "127.0.0.1"
    service_max_concurrency: int = 16
    # S3-A Telegram registration binding: fail closed when the bot username/shared key are
    # absent (the route is then disabled and /me reports registration state none).
    telegram_bot_username: str = ""
    telegram_bot_key: str = ""
    telegram_bot_token: str = ""
    telegram_trial_channel_id: str = ""
    registration_token_ttl_seconds: int = 600
    # S4 payment slice: server-authoritative prices/tariff and Platega provider configuration.
    payment_currency: str = "RUB"
    payment_tariff_key: str = "terlimo-200-30d-v1"
    payment_price_rub_1: int = 0
    payment_price_rub_3: int = 0
    payment_price_rub_6: int = 0
    # S5 only; empty account disables buyer-specific low-value offers.
    s5_control_account_id: str = ""
    s5_control_price_rub_1: int = 0
    s5_control_price_rub_3: int = 0
    s5_control_price_rub_extra: int = 0
    platega_owner_routing_enabled: bool = False
    platega_old_ownership_url: str = ""
    platega_old_webhook_url: str = ""
    platega_owner_routing_timeout_seconds: int = 10
    platega_enabled: bool = False
    platega_base_url: str = "https://app.platega.io"
    platega_merchant_id: str = ""
    platega_secret: str = ""
    platega_return_url: str = ""
    platega_failed_url: str = ""
    platega_methods: str = "sbp,international,crypto"
    platega_method_ids: str = "sbp:2,international:12,crypto:13"
    platega_http_timeout_seconds: int = 10

    @property
    def redacted_database_url(self) -> str:
        return redacted_database_url(self.database_url)


def load_settings(require_database: bool = True) -> Settings:
    database_url = _env_str("DATABASE_URL")
    if require_database and not database_url:
        raise ConfigError(
            "DATABASE_URL is not set. The backend never falls back to another database; "
            "provide an isolated TEST PostgreSQL DSN."
        )
    if database_url and not database_url.startswith(("postgresql://", "postgres://")):
        raise ConfigError("DATABASE_URL must be a postgresql:// DSN")
    settings = Settings(
        database_url=database_url or "",
        environment=_env_str("TERLIMO_ENV", "test") or "test",
        log_level=(_env_str("LOG_LEVEL", "INFO") or "INFO").upper(),
        api_host=_env_str("API_HOST", "127.0.0.1") or "127.0.0.1",
        api_port=_env_int("API_PORT", 18081, 1),
        db_pool_min=_env_int("DB_POOL_MIN", 1, 0),
        db_pool_max=_env_int("DB_POOL_MAX", 5, 1),
        db_command_timeout_seconds=_env_int("DB_COMMAND_TIMEOUT_SECONDS", 15, 1),
        db_reconnect_cooldown_seconds=_env_float("DB_RECONNECT_COOLDOWN_SECONDS", 1.0, 0.0),
        worker_poll_interval_seconds=_env_float("WORKER_POLL_INTERVAL_SECONDS", 1.0, 0.05),
        worker_lock_timeout_seconds=_env_int("WORKER_LOCK_TIMEOUT_SECONDS", 30, 1),
        worker_max_attempts=_env_int("WORKER_MAX_ATTEMPTS", 8, 1),
        challenge_ttl_seconds=_env_int("CHALLENGE_TTL_SECONDS", 300, 1),
        session_ttl_seconds=_env_int("SESSION_TTL_SECONDS", 86400, 60),
        proof_skew_seconds=_env_int("PROOF_SKEW_SECONDS", 300, 0),
        challenge_rate_limit_per_minute=_env_int("CHALLENGE_RATE_LIMIT_PER_MINUTE", 20, 1),
        public_challenge_limit_per_minute=_env_int("PUBLIC_CHALLENGE_LIMIT_PER_MINUTE", 20, 1),
        challenge_retention_seconds=_env_int("CHALLENGE_RETENTION_SECONDS", 86400, 0),
        receipt_result_ttl_seconds=_env_int("RECEIPT_RESULT_TTL_SECONDS", 3600, 1),
        idempotency_window_seconds=_env_int("IDEMPOTENCY_WINDOW_SECONDS", 604800, 60),
        cleanup_interval_seconds=_env_int("CLEANUP_INTERVAL_SECONDS", 300, 5),
        cleanup_batch_size=_env_int("CLEANUP_BATCH_SIZE", 500, 1),
        reminders_enabled=_env_bool("REMINDERS_ENABLED", True),
        reminder_window_seconds=_env_int("REMINDER_WINDOW_SECONDS", 259200, 60),
        telegram_notifications_enabled=_env_bool("TELEGRAM_NOTIFICATIONS_ENABLED", False),
        catalog_validity_seconds=_env_int("CATALOG_VALIDITY_SECONDS", 600, 60),
        gateway_admin_main_password=_env_str("GATEWAY_ADMIN_MAIN_PASSWORD", "") or "",
        gateway_admin_timeout_seconds=_env_int("GATEWAY_ADMIN_TIMEOUT_SECONDS", 10, 1),
        onboarding_secret_key=_env_str("ONBOARDING_SECRET_KEY", "") or "",
        gateway_local_admin_enabled=_env_bool("GATEWAY_LOCAL_ADMIN_ENABLED", False),
        gateway_management_ca_file=_env_str("GATEWAY_MANAGEMENT_CA_FILE", "") or "",
        gateway_management_cert_file=_env_str("GATEWAY_MANAGEMENT_CERT_FILE", "") or "",
        gateway_management_key_file=_env_str("GATEWAY_MANAGEMENT_KEY_FILE", "") or "",
        gateway_max_lease_seconds=_env_int("GATEWAY_MAX_LEASE_SECONDS", 900, 30),
        evidence_endpoint_enabled=_env_bool("ONBOARDING_EVIDENCE_ENDPOINT_ENABLED", False),
        evidence_server_cert_file=_env_str("ONBOARDING_EVIDENCE_SERVER_CERT_FILE", "") or "",
        evidence_server_key_file=_env_str("ONBOARDING_EVIDENCE_SERVER_KEY_FILE", "") or "",
        evidence_client_ca_file=_env_str("ONBOARDING_EVIDENCE_CLIENT_CA_FILE", "") or "",
        evidence_listen_host=_env_str("ONBOARDING_EVIDENCE_LISTEN_HOST", "127.0.0.1") or "127.0.0.1",
        evidence_listen_port=_env_int("ONBOARDING_EVIDENCE_LISTEN_PORT", 0, 0),
        evidence_relay_enabled=_env_bool("ONBOARDING_EVIDENCE_RELAY_ENABLED", False),
        evidence_node_cert_file=_env_str("ONBOARDING_EVIDENCE_NODE_CERT_FILE", "") or "",
        evidence_node_key_file=_env_str("ONBOARDING_EVIDENCE_NODE_KEY_FILE", "") or "",
        evidence_backend_ca_file=_env_str("ONBOARDING_EVIDENCE_BACKEND_CA_FILE", "") or "",
        evidence_backend_host=_env_str("ONBOARDING_EVIDENCE_BACKEND_HOST", "") or "",
        evidence_backend_port=_env_int("ONBOARDING_EVIDENCE_BACKEND_PORT", 0, 0),
        evidence_backend_server_name=_env_str("ONBOARDING_EVIDENCE_BACKEND_SERVER_NAME", "") or "",
        evidence_relay_socket=_env_str("ONBOARDING_EVIDENCE_RELAY_SOCKET", "") or "",
        evidence_relay_allowed_uid=_env_int("ONBOARDING_EVIDENCE_RELAY_UID", -1, -1),
        evidence_relay_allowed_gid=_env_int("ONBOARDING_EVIDENCE_RELAY_GID", -1, -1),
        evidence_timeout_seconds=_env_int("ONBOARDING_EVIDENCE_TIMEOUT_SECONDS", 15, 1),
        service_endpoint_enabled=_env_bool("ONBOARDING_SERVICE_ENDPOINT_ENABLED", False),
        service_relay_enabled=_env_bool("ONBOARDING_SERVICE_RELAY_ENABLED", False),
        service_relay_socket=_env_str("ONBOARDING_SERVICE_RELAY_SOCKET", "") or "",
        service_relay_allowed_uid=_env_int("ONBOARDING_SERVICE_RELAY_UID", -1, -1),
        service_relay_allowed_gid=_env_int("ONBOARDING_SERVICE_RELAY_GID", -1, -1),
        service_timeout_seconds=_env_int("ONBOARDING_SERVICE_TIMEOUT_SECONDS", 15, 1),
        service_upstream_host=_env_str("ONBOARDING_SERVICE_UPSTREAM_HOST", "127.0.0.1") or "127.0.0.1",
        service_max_concurrency=_env_int("ONBOARDING_SERVICE_MAX_CONCURRENCY", 16, 1),
        telegram_bot_username=_env_str("TELEGRAM_REGISTRATION_BOT_USERNAME", "") or "",
        telegram_bot_key=_env_str("TELEGRAM_REGISTRATION_BOT_KEY", "") or "",
        telegram_bot_token=_env_str("TELEGRAM_REGISTRATION_BOT_TOKEN", "") or "",
        telegram_trial_channel_id=_env_str("TELEGRAM_TRIAL_CHANNEL_ID", "") or "",
        registration_token_ttl_seconds=_env_int("TELEGRAM_REGISTRATION_TOKEN_TTL_SECONDS", 600, 60),
        payment_currency=_env_str("PAYMENT_CURRENCY", "RUB") or "RUB",
        payment_tariff_key=_env_str("PAYMENT_TARIFF_KEY", "terlimo-200-30d-v1") or "terlimo-200-30d-v1",
        payment_price_rub_1=_env_int("PAYMENT_PRICE_RUB_1", 0, 0),
        payment_price_rub_3=_env_int("PAYMENT_PRICE_RUB_3", 0, 0),
        payment_price_rub_6=_env_int("PAYMENT_PRICE_RUB_6", 0, 0),
        s5_control_account_id=_env_str("S5_CONTROL_ACCOUNT_ID", "") or "",
        s5_control_price_rub_1=_env_int("S5_CONTROL_PRICE_RUB_1", 0, 0),
        s5_control_price_rub_3=_env_int("S5_CONTROL_PRICE_RUB_3", 0, 0),
        s5_control_price_rub_extra=_env_int("S5_CONTROL_PRICE_RUB_EXTRA", 0, 0),
        platega_owner_routing_enabled=(_env_str("PLATEGA_OWNER_ROUTING_ENABLED", "") or "").lower() in ("1", "true", "yes", "on"),
        platega_old_ownership_url=_env_str("PLATEGA_OLD_OWNERSHIP_URL", "") or "",
        platega_old_webhook_url=_env_str("PLATEGA_OLD_WEBHOOK_URL", "") or "",
        platega_owner_routing_timeout_seconds=_env_int("PLATEGA_OWNER_ROUTING_TIMEOUT_SECONDS", 10, 1),
        platega_enabled=(_env_str("PLATEGA_ENABLED", "") or "").lower() in ("1", "true", "yes", "on"),
        platega_base_url=_env_str("PLATEGA_BASE_URL", "https://app.platega.io") or "https://app.platega.io",
        platega_merchant_id=_env_str("PLATEGA_MERCHANT_ID", "") or "",
        platega_secret=_env_str("PLATEGA_SECRET", "") or "",
        platega_return_url=_env_str("PLATEGA_RETURN_URL", "") or "",
        platega_failed_url=_env_str("PLATEGA_FAILED_URL", "") or "",
        platega_methods=_env_str("PLATEGA_METHODS", "sbp,international,crypto") or "sbp,international,crypto",
        platega_method_ids=_env_str("PLATEGA_METHOD_IDS", "sbp:2,international:12,crypto:13") or "sbp:2,international:12,crypto:13",
        platega_http_timeout_seconds=_env_int("PLATEGA_HTTP_TIMEOUT_SECONDS", 10, 1),
    )
    if settings.db_pool_min > settings.db_pool_max:
        raise ConfigError("DB_POOL_MIN must not exceed DB_POOL_MAX")
    if settings.evidence_endpoint_enabled:
        required = {
            "ONBOARDING_EVIDENCE_SERVER_CERT_FILE": settings.evidence_server_cert_file,
            "ONBOARDING_EVIDENCE_SERVER_KEY_FILE": settings.evidence_server_key_file,
            "ONBOARDING_EVIDENCE_CLIENT_CA_FILE": settings.evidence_client_ca_file,
            "ONBOARDING_EVIDENCE_LISTEN_HOST": settings.evidence_listen_host,
        }
        missing = sorted(name for name, value in required.items() if not value)
        if missing:
            raise ConfigError(
                "ONBOARDING_EVIDENCE_ENDPOINT_ENABLED requires " + ", ".join(missing)
            )
    if settings.evidence_relay_enabled:
        required = {
            "ONBOARDING_EVIDENCE_NODE_CERT_FILE": settings.evidence_node_cert_file,
            "ONBOARDING_EVIDENCE_NODE_KEY_FILE": settings.evidence_node_key_file,
            "ONBOARDING_EVIDENCE_BACKEND_CA_FILE": settings.evidence_backend_ca_file,
            "ONBOARDING_EVIDENCE_BACKEND_HOST": settings.evidence_backend_host,
            "ONBOARDING_EVIDENCE_BACKEND_SERVER_NAME": settings.evidence_backend_server_name,
            "ONBOARDING_EVIDENCE_RELAY_SOCKET": settings.evidence_relay_socket,
        }
        missing = sorted(name for name, value in required.items() if not value)
        if missing:
            raise ConfigError(
                "ONBOARDING_EVIDENCE_RELAY_ENABLED requires " + ", ".join(missing)
            )
        if not settings.evidence_backend_port:
            raise ConfigError("ONBOARDING_EVIDENCE_RELAY_ENABLED requires ONBOARDING_EVIDENCE_BACKEND_PORT")
        if settings.evidence_relay_allowed_uid < 0:
            raise ConfigError("ONBOARDING_EVIDENCE_RELAY_ENABLED requires ONBOARDING_EVIDENCE_RELAY_UID >= 0")
    if settings.service_endpoint_enabled:
        required = {
            "ONBOARDING_EVIDENCE_SERVER_CERT_FILE": settings.evidence_server_cert_file,
            "ONBOARDING_EVIDENCE_SERVER_KEY_FILE": settings.evidence_server_key_file,
            "ONBOARDING_EVIDENCE_CLIENT_CA_FILE": settings.evidence_client_ca_file,
            "ONBOARDING_EVIDENCE_LISTEN_HOST": settings.evidence_listen_host,
        }
        missing = sorted(name for name, value in required.items() if not value)
        if missing:
            raise ConfigError(
                "ONBOARDING_SERVICE_ENDPOINT_ENABLED requires " + ", ".join(missing)
            )
    if settings.service_relay_enabled:
        required = {
            "ONBOARDING_EVIDENCE_NODE_CERT_FILE": settings.evidence_node_cert_file,
            "ONBOARDING_EVIDENCE_NODE_KEY_FILE": settings.evidence_node_key_file,
            "ONBOARDING_EVIDENCE_BACKEND_CA_FILE": settings.evidence_backend_ca_file,
            "ONBOARDING_EVIDENCE_BACKEND_HOST": settings.evidence_backend_host,
            "ONBOARDING_EVIDENCE_BACKEND_SERVER_NAME": settings.evidence_backend_server_name,
            "ONBOARDING_SERVICE_RELAY_SOCKET": settings.service_relay_socket,
        }
        missing = sorted(name for name, value in required.items() if not value)
        if missing:
            raise ConfigError(
                "ONBOARDING_SERVICE_RELAY_ENABLED requires " + ", ".join(missing)
            )
        if not settings.evidence_backend_port:
            raise ConfigError("ONBOARDING_SERVICE_RELAY_ENABLED requires ONBOARDING_EVIDENCE_BACKEND_PORT")
        if settings.service_relay_allowed_uid < 0:
            raise ConfigError("ONBOARDING_SERVICE_RELAY_ENABLED requires ONBOARDING_SERVICE_RELAY_UID >= 0")
    if settings.service_endpoint_enabled and settings.service_upstream_host not in ("127.0.0.1", "::1", "localhost"):
        raise ConfigError("ONBOARDING_SERVICE_UPSTREAM_HOST must be a fixed loopback address")
    return settings
