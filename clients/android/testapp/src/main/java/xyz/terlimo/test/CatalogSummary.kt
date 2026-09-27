package xyz.terlimo.test

import org.json.JSONObject
import java.time.Instant
import java.time.ZoneId
import java.time.format.DateTimeFormatter

/** Display-only projection of the native authenticated catalog, never an admission gate. */
internal data class CatalogSummary(
    val subscriptionExpiresAt: Instant?,
    val catalogExpiresAt: Instant,
    val issuedAt: Instant,
    val slotsUsed: Int,
    val slotsLimit: Int,
) {
    fun description(now: Instant = Instant.now(), zone: ZoneId = ZoneId.systemDefault()): String {
        val format = DateTimeFormatter.ofPattern("dd.MM.yyyy HH:mm:ss z").withZone(zone)
        val subscription = when {
            subscriptionExpiresAt == null -> "Подписка без ограничения срока"
            now >= subscriptionExpiresAt -> "Срок подписки истёк по последним данным"
            else -> "Подписка до ${format.format(subscriptionExpiresAt)}"
        }
        val catalog = if (now >= catalogExpiresAt) "Срок каталога истёк; перед VPN требуется обновление" else "Каталог до ${format.format(catalogExpiresAt)}"
        return "$subscription\nМеста: $slotsUsed из $slotsLimit\nДанные сервера от ${format.format(issuedAt)}\n$catalog\nЭто последние подтверждённые данные; статус доступа проверяет сервер. Срок каталога не является сроком подписки."
    }

    companion object {
        fun parse(event: JSONObject): CatalogSummary {
            fun stamp(key: String): Instant {
                val value = event.opt(key)
                require(value is String && value.length <= 40) { "CATALOG_INVALID" }
                return Instant.parse(value)
            }
            fun count(key: String): Int {
                val value = event.opt(key)
                require(value is Int || value is Long) { "CATALOG_INVALID" }
                val number = (value as Number).toLong()
                require(number in 1..Int.MAX_VALUE.toLong()) { "CATALOG_INVALID" }
                return number.toInt()
            }
            require(event.opt("subscription_status") == "active") { "CATALOG_INVALID" }
            val version = event.optInt("catalog_version", 1)
            val unlimited = event.opt("subscription_unlimited") == true
            val subscription = if (version == 2 && unlimited && event.has("subscription_expires_at") && event.isNull("subscription_expires_at")) null else stamp("subscription_expires_at")
            require((version == 1 && !unlimited && subscription != null) || (version == 2 && unlimited && subscription == null)) { "CATALOG_INVALID" }
            val result = CatalogSummary(subscription, stamp("catalog_expires_at"), stamp("issued_at"), count("slots_used"), count("slots_limit"))
            require(result.slotsUsed <= result.slotsLimit && result.issuedAt < result.catalogExpiresAt && (result.subscriptionExpiresAt == null || result.catalogExpiresAt <= result.subscriptionExpiresAt)) { "CATALOG_INVALID" }
            return result
        }
    }
}
