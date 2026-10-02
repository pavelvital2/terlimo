"""Commercial snapshots for the existing order/credit pipeline; no provider calls."""
from __future__ import annotations
import json
import uuid
from datetime import UTC, datetime, timedelta
from .auth_api import ApiError, rfc3339

ADDON_PLAN = "terlimo-extra-device"
MONTH_SECONDS = 30 * 86400


def product_of(row):
    raw = row["product"] if "product" in row else None
    if isinstance(raw, str):
        raw = json.loads(raw)
    return raw


def order_product(row):
    raw = row["quote"]
    if isinstance(raw, str):
        raw = json.loads(raw)
    return raw.get("product") if isinstance(raw, dict) else None


async def active_paid(connection, account_id, now=None):
    now = now or datetime.now(UTC)
    if account_id is None:
        return None
    return await connection.fetchrow("""SELECT * FROM entitlements
        WHERE account_id=$1 AND kind='paid' AND status='active'
        AND (starts_at IS NULL OR starts_at <= $2) AND ends_at > $2
        ORDER BY created_at DESC LIMIT 1""", account_id, now)


async def slots(connection, entitlement_id, now=None):
    return await connection.fetch("""SELECT id, expires_at FROM paid_extra_slots
        WHERE entitlement_id=$1 AND expires_at > $2 ORDER BY id""",
        entitlement_id, now or datetime.now(UTC))


async def paid_limit(connection, entitlement, now=None):
    if entitlement["kind"] != "paid" or entitlement["ends_at"] is None:
        return int(entitlement["device_limit"] or 2)
    return int(entitlement["paid_base_device_limit"]) + len(await slots(connection, entitlement["id"], now))


def _micros(delta):
    return max(0,(delta.days*86400+delta.seconds)*1000000+delta.microseconds)


def prorata_minor(end, valid_from, now, months):
    """Owner policy: floor remaining whole days, then round whole RUB nearest, half up."""
    months = max(1,months)
    days_basis = months*30
    remaining_days = _micros(end-max(now,valid_from or now))//(86400*1000000)
    full_minor = 10000*months
    numerator = full_minor*min(remaining_days,days_basis)
    denominator = 100*days_basis
    return 100*((2*numerator+denominator)//(2*denominator))


def price_months(target):
    raw = target["source_plan"]
    if isinstance(raw,str):
        raw = json.loads(raw)
    code = raw.get("duration_code") if isinstance(raw,dict) else None
    if code == "days:30":
        return 1
    if code in ("months:3","months:6"):
        return int(code.split(":")[1])
    # Legacy finite rights without a source plan: basis follows their stored real interval.
    # It is recorded in the quote; never silently clamp all durations to one month.
    span = _micros(target["ends_at"]-(target["starts_at"] or datetime.now(UTC)))
    return max(1,(span+MONTH_SECONDS*1000000-1)//(MONTH_SECONDS*1000000))


async def quote_product(connection, settings, account_id, *, addon, selected, months, base_amount_minor, now=None):
    now = now or datetime.now(UTC)
    target = await active_paid(connection,account_id,now)
    if addon and target is None:
        raise ApiError("PAYMENT_STATE_INVALID",http=409)
    available = await slots(connection,target["id"],now) if target else []
    try:
        chosen = [uuid.UUID(x) for x in selected]
    except (ValueError,TypeError,AttributeError):
        raise ApiError("BAD_MESSAGE",http=400) from None
    if len(chosen) != len(set(chosen)) or not set(chosen) <= {x["id"] for x in available} or (addon and chosen):
        raise ApiError("BAD_MESSAGE",http=400)
    control = bool(account_id) and bool(settings.s5_control_account_id) and uuid.UUID(str(account_id)) == uuid.UUID(settings.s5_control_account_id)
    override_minor = settings.s5_control_price_rub_extra*100 if control else 0
    basis_months = price_months(target) if target else max(1,months)
    extra_views = []
    gap_basis = []
    for slot in available:
        gap_minor = prorata_minor(target["ends_at"],slot["expires_at"],now,basis_months)
        renew_minor = override_minor or (10000*months+gap_minor)
        gap_basis.append({"slot_id":str(slot["id"]),"valid_from":rfc3339(slot["expires_at"]),"remaining_full_days":_micros(target["ends_at"]-max(now,slot["expires_at"]))//(86400*1000000),"gap_amount_minor":gap_minor,"renew_amount_minor":renew_minor})
        extra_views.append({"slot_id":str(slot["id"]),"expires_at":rfc3339(slot["expires_at"]),"renew_amount_minor":None if addon else renew_minor})
    if addon:
        extra_minor = override_minor or prorata_minor(target["ends_at"],target["starts_at"],now,basis_months)
        amount_minor = extra_minor
        limit = int(target["paid_base_device_limit"])+len(available)+1
        period_from,period_until,plan_id = now,target["ends_at"],ADDON_PLAN
    else:
        extra_minor = sum(slot["renew_amount_minor"] for slot in extra_views if uuid.UUID(slot["slot_id"]) in chosen)
        amount_minor = base_amount_minor+extra_minor
        limit = 2+len(chosen)
        period_from = target["ends_at"] if target else now
        from .payments import paid_end,PLAN_SNAPSHOTS
        period_until = paid_end(period_from,{"unit":"days" if months==1 else "months","value":30 if months==1 else months})
        plan_id = PLAN_SNAPSHOTS[months]["plan_id"]
    basis = None
    if target:
        basis = {"period_months":basis_months,"full_amount_minor":10000*basis_months,"month_seconds":MONTH_SECONDS,"valid_from":rfc3339(target["starts_at"]) if target["starts_at"] else None,"valid_until":rfc3339(target["ends_at"]),"remaining_microseconds":_micros(target["ends_at"]-max(now,target["starts_at"] or now)),"rounding":"nearest_rub_half_up","remaining_day_policy":"floor_full_86400_seconds","remaining_full_days":_micros(target["ends_at"]-max(now,target["starts_at"] or now))//(86400*1000000),"period_cap_days":30*basis_months,"charged_days":min(_micros(target["ends_at"]-max(now,target["starts_at"] or now))//(86400*1000000),30*basis_months),"ordinary_addon_amount_minor":prorata_minor(target["ends_at"],target["starts_at"],now,basis_months),"quoted_amount_minor":amount_minor,"quoted_extra_amount_minor":extra_minor,"selected_gap_basis":gap_basis,"control_override_minor":override_minor or None}
    return amount_minor,limit,{
        "kind":"device_addon" if addon else "subscription","plan_id":plan_id,
        "device_delta":1 if addon else 0,
        "target_entitlement_id":str(target["id"]) if target else None,
        "owner_account_id":str(account_id) if account_id else None,
        "target_valid_until":rfc3339(target["ends_at"]) if target else None,
        "valid_from":rfc3339(period_from),"valid_until":rfc3339(period_until),
        "renew_extra_slot_ids":[str(x) for x in sorted(chosen)],
        "base_amount_minor":0 if addon else base_amount_minor,"extra_amount_minor":extra_minor,
        "device_limit":limit,"extra_slots":extra_views,"extra_price_basis":basis,
    }


def public_product(product):
    return {k: v for k, v in product.items() if k not in ("owner_account_id", "extra_price_basis")}


async def validate_credit_target(connection, order, binding, now):
    product = order_product(order)
    if not product:
        return None, None
    if not binding or str(binding["account_id"]) != product["owner_account_id"] or (
        order["checkout_owner_binding_id"] != binding["id"]
    ):
        return None, "owner_changed"
    target_id = product["target_entitlement_id"]
    if not target_id:
        return None, None
    target = await connection.fetchrow("SELECT * FROM entitlements WHERE id=$1 FOR UPDATE", uuid.UUID(target_id))
    if target is None or target["account_id"] != binding["account_id"] or target["kind"] != 'paid' or target["status"] != 'active':
        return None, "target_unavailable"
    if product["kind"] == "device_addon":
        if target["ends_at"] is None or target["ends_at"] <= now or (target["starts_at"] and target["starts_at"] > now):
            return None, "target_expired"
        if rfc3339(target["ends_at"]) != product["valid_until"]:
            return None, "target_period_changed"
    selected = [uuid.UUID(x) for x in product["renew_extra_slot_ids"]]
    if selected:
        count = await connection.fetchval("SELECT count(*) FROM paid_extra_slots WHERE entitlement_id=$1 AND id=ANY($2::uuid[])",target["id"],selected)
        if count != len(selected):
            return None, "extra_slot_unavailable"
    return target, None


async def renew_slots(connection, order, entitlement_id, new_end):
    product = order_product(order)
    selected = product["renew_extra_slot_ids"] if product else []
    if selected:
        await connection.execute("UPDATE paid_extra_slots SET expires_at=$3 WHERE entitlement_id=$1 AND id=ANY($2::uuid[])",entitlement_id,[uuid.UUID(x) for x in selected],new_end)


async def refresh_expired_limits(connection, *, limit=100):
    # Existing maintenance loop: materialize only changed limits; reads below also derive expiry exactly.
    async with connection.transaction():
        rows = await connection.fetch("""SELECT e.* FROM entitlements e WHERE e.kind='paid' AND e.ends_at IS NOT NULL
           AND e.device_limit <> e.paid_base_device_limit + (SELECT count(*) FROM paid_extra_slots s WHERE s.entitlement_id=e.id AND s.expires_at>now())
           ORDER BY e.id LIMIT $1 FOR UPDATE OF e SKIP LOCKED""",limit)
        for row in rows:
            value = await paid_limit(connection,row)
            await connection.execute("UPDATE entitlements SET device_limit=$2,revision=revision+1 WHERE id=$1",row["id"],value)
        return len(rows)


async def binding_paid_capacity(connection, entitlement, binding_id, now=None):
    """TZ15.7: first N active bound_at/id bindings retain access; no slot/device assignment.

    Return eligibility and this binding's commercial deadline. An extra-ranked binding
    gets the rank-th latest extra expiry, so renewal of any extra preserves first N.
    Non-finite/non-paid rights retain their existing policy.
    """
    now = now or datetime.now(UTC)
    if entitlement["kind"] != "paid" or entitlement["ends_at"] is None:
        return True, entitlement["ends_at"]
    bound = await connection.fetch("SELECT id FROM account_bindings WHERE account_id=$1 AND status='active' ORDER BY bound_at,id",entitlement["account_id"])
    ranks = {row["id"]:i+1 for i,row in enumerate(bound)}
    rank = ranks.get(binding_id)
    if rank is None:
        return False, now
    base = int(entitlement["paid_base_device_limit"])
    if rank <= base:
        return True, entitlement["ends_at"]
    extra = sorted((row["expires_at"] for row in await slots(connection,entitlement["id"],now)),reverse=True)
    if rank-base-1 >= len(extra):
        return False, now
    deadline = min(entitlement["ends_at"],extra[rank-base-1])
    return deadline > now, deadline


async def current_binding_paid_capacity(connection, binding_id, now=None):
    """Fresh worker fence: authoritative active commercial right, no stale queued lease."""
    now = now or datetime.now(UTC)
    entitlement = await connection.fetchrow("""SELECT e.* FROM entitlements e
        JOIN account_bindings b ON b.account_id=e.account_id
        WHERE b.id=$1 AND b.status='active' AND e.kind IN ('paid','trial','imported')
        AND e.status='active' AND (e.starts_at IS NULL OR e.starts_at <= $2)
        AND (e.ends_at IS NULL OR e.ends_at > $2)
        ORDER BY e.created_at DESC LIMIT 1""",binding_id,now)
    if entitlement is None:
        return False, now
    return await binding_paid_capacity(connection,entitlement,binding_id,now)


async def cap_existing_extra_grants(connection, *, max_lease_seconds, limit=100):
    """Existing maintenance/outbox: shorten already-issued extra leases before paid expiry.

    Select only a concrete overlong lease; retained base bindings never enter this sweep.
    Expired rights cannot be refreshed: an old uncapped lease ends at its stored bound.
    """
    rows = await connection.fetch("""WITH ranked AS (
        SELECT id,account_id,row_number() OVER (PARTITION BY account_id ORDER BY bound_at,id) AS rank
        FROM account_bindings WHERE status='active'
    ) SELECT b.id AS binding_id,g.gateway_id,e.id AS entitlement_id
    FROM ranked b
    JOIN grants g ON g.binding_id=b.id AND g.state IN ('pending','applying','applied')
    JOIN LATERAL (
        SELECT * FROM entitlements WHERE account_id=b.account_id
        AND kind IN ('paid','trial','imported') AND status='active'
        AND (starts_at IS NULL OR starts_at<=now()) AND (ends_at IS NULL OR ends_at>now())
        ORDER BY created_at DESC LIMIT 1
    ) e ON e.kind='paid' AND e.ends_at IS NOT NULL
    JOIN LATERAL (
        SELECT expires_at FROM paid_extra_slots WHERE entitlement_id=e.id AND expires_at>now()
        ORDER BY expires_at DESC OFFSET GREATEST(0,b.rank-e.paid_base_device_limit-1) LIMIT 1
    ) s ON true
    WHERE b.rank>e.paid_base_device_limit AND g.not_after>LEAST(e.ends_at,s.expires_at)
    ORDER BY s.expires_at,g.id LIMIT $1""",limit)
    from .gateway_control import ensure_grant
    count = 0
    for row in rows:
        async with connection.transaction():
            outcome = await ensure_grant(connection,binding_id=row['binding_id'],gateway_id=row['gateway_id'],entitlement_id=row['entitlement_id'],max_lease_seconds=max_lease_seconds)
        count += outcome == 'enqueued'
    return count
