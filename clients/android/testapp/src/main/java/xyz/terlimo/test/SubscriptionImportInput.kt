package xyz.terlimo.test

import java.net.URI

object SubscriptionImportInput {
    private const val MAX_LENGTH = 65_536

    fun normalize(raw: String): String {
        require(raw.toByteArray(Charsets.UTF_8).size in 1..MAX_LENGTH) { "IMPORT_LINK_INVALID" }
        val value = raw.trim()
        require(value.isNotEmpty()) { "IMPORT_LINK_INVALID" }
        require(value.none { it.isWhitespace() || it.code < 0x20 || it.code == 0x7f }) {
            "IMPORT_LINK_INVALID"
        }
        val uri = URI(value)
        require(uri.scheme == "whitelists" && uri.host == "subscription") {
            "IMPORT_LINK_INVALID"
        }
        require(uri.userInfo == null && uri.port == -1 && uri.path.orEmpty().isEmpty()) {
            "IMPORT_LINK_INVALID"
        }
        require(uri.rawQuery == "v=1" && !uri.rawFragment.isNullOrEmpty()) { "IMPORT_LINK_INVALID" }
        return value
    }
}
