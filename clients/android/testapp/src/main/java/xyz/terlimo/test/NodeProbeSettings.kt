package xyz.terlimo.test

import org.json.JSONObject
import java.net.URI

internal data class NodeProbe(val probeUrl: String, val expectedExitIp: String)

/** Immutable public probe metadata, explicitly bound to a catalog node ID. */
internal class NodeProbeSettings private constructor(
    private val probes: Map<String, NodeProbe>,
) {
    operator fun get(nodeId: String): NodeProbe? = probes[nodeId]

    fun toNativeJson(): JSONObject = JSONObject().also { root ->
        probes.forEach { (nodeId, probe) ->
            root.put(nodeId, JSONObject()
                .put("probe_url", probe.probeUrl)
                .put("expected_exit_ip", probe.expectedExitIp))
        }
    }

    companion object {
        fun parse(probeDocument: String, immutableTrustDocument: String): NodeProbeSettings {
            val root = JSONObject(probeDocument)
            val keys = root.keys().asSequence().toSet()
            val probes = when (keys) {
                setOf("probe_url", "expected_exit_ip") -> {
                    val trust = JSONObject(immutableTrustDocument)
                    val nodeId = strictString(trust, "node_id").also { check(it.isNotEmpty()) { "PROBE_INVALID" } }
                    linkedMapOf(nodeId to parseProbe(root))
                }
                setOf("nodes") -> parseExplicitNodes(root.get("nodes"))
                else -> error("PROBE_INVALID")
            }
            return NodeProbeSettings(probes.toMap())
        }

        private fun parseExplicitNodes(value: Any): LinkedHashMap<String, NodeProbe> {
            val nodes = value as? JSONObject ?: error("PROBE_INVALID")
            // Technical bound only: the probe map follows the arbitrary verified
            // gateway catalog and must never truncate it.
            check(nodes.length() in 1..1024) { "PROBE_INVALID" }
            val result = linkedMapOf<String, NodeProbe>()
            nodes.keys().forEach { nodeId ->
                check(nodeId.isNotEmpty()) { "PROBE_INVALID" }
                val item = nodes.get(nodeId) as? JSONObject ?: error("PROBE_INVALID")
                check(item.keys().asSequence().toSet() == setOf("probe_url", "expected_exit_ip")) { "PROBE_INVALID" }
                result[nodeId] = parseProbe(item)
            }
            return result
        }

        private fun parseProbe(value: JSONObject): NodeProbe {
            val url = strictString(value, "probe_url")
            val expectedIp = strictString(value, "expected_exit_ip")
            check(validHttpsUrl(url) && isIpLiteral(expectedIp)) { "PROBE_INVALID" }
            return NodeProbe(url, expectedIp)
        }

        private fun strictString(value: JSONObject, key: String): String =
            value.get(key) as? String ?: error("PROBE_INVALID")

        private fun validHttpsUrl(value: String): Boolean = runCatching {
            val uri = URI(value)
            uri.scheme.equals("https", ignoreCase = true) && uri.host != null &&
                uri.rawUserInfo == null && uri.rawFragment == null && uri.port in -1..65535
        }.getOrDefault(false)

        private fun isIpLiteral(value: String): Boolean = isIpv4Literal(value) || isIpv6Literal(value)

        private fun isIpv4Literal(value: String): Boolean {
            val parts = value.split('.')
            return parts.size == 4 && parts.all { part ->
                part.isNotEmpty() && part.length <= 3 && part.all { it in '0'..'9' } &&
                    (part.length == 1 || part[0] != '0') && part.toInt() in 0..255
            }
        }

        private fun isIpv6Literal(value: String): Boolean {
            if (!value.contains(':') || value.contains('%') || value.contains(":::")) return false
            val compression = value.indexOf("::")
            if (compression >= 0 && value.indexOf("::", compression + 2) >= 0) return false
            if (compression < 0 && (value.startsWith(':') || value.endsWith(':'))) return false

            val left = if (compression < 0) value else value.substring(0, compression)
            val right = if (compression < 0) "" else value.substring(compression + 2)
            val groups = (left.split(':').filter(String::isNotEmpty) +
                right.split(':').filter(String::isNotEmpty))
            var count = 0
            groups.forEachIndexed { index, group ->
                if (group.contains('.')) {
                    if (index != groups.lastIndex || !isIpv4Literal(group)) return false
                    count += 2
                } else {
                    if (group.length !in 1..4 || !group.all { it.isDigit() || it.lowercaseChar() in 'a'..'f' }) return false
                    count++
                }
            }
            return if (compression >= 0) count < 8 else count == 8
        }
    }
}
