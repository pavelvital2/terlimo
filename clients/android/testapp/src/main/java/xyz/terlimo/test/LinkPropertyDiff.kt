package xyz.terlimo.test

/** Values stay in memory; reports contain only the names of changed field categories. */
internal object LinkPropertyDiff {
    val fields = setOf("INTERFACE", "ADDRESSES", "DNS", "ROUTES", "DOMAINS", "MTU", "HTTP_PROXY", "PRIVATE_DNS_ACTIVE", "PRIVATE_DNS_NAME", "NAT64")
    fun changed(before: Map<String, Any?>?, after: Map<String, Any?>?, exactEqual: Boolean): Set<String> {
        if (before == null || after == null) return if (before == after) emptySet() else setOf("NETWORK_UNAVAILABLE")
        val result = fields.filterTo(linkedSetOf()) { before[it] != after[it] }
        if (!exactEqual && result.isEmpty()) result.add("UNOBSERVED_FIELDS")
        return result
    }
}
