"""Configuration: required database, DSN validation and secret-free redaction."""

from __future__ import annotations

import pytest

from terlimo_backend.config import ConfigError, load_settings, redacted_database_url


def test_missing_database_url_is_explicit(monkeypatch):
    monkeypatch.delenv("DATABASE_URL", raising=False)
    with pytest.raises(ConfigError, match="DATABASE_URL"):
        load_settings()


def test_non_postgres_dsn_is_rejected(monkeypatch):
    monkeypatch.setenv("DATABASE_URL", "sqlite:///tmp/test.db")
    with pytest.raises(ConfigError):
        load_settings()


def test_database_url_redaction_hides_password():
    redacted = redacted_database_url(
        "postgresql://terlimo_backend:supersecret@db.internal:5432/terlimo_test"
    )
    assert "supersecret" not in redacted
    assert "db.internal:5432" in redacted
    assert "terlimo_test" in redacted
