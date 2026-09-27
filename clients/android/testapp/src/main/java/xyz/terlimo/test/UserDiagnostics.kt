package xyz.terlimo.test

/** Bounded local UI text. No raw map, payload, names, addresses or credentials. */
internal object UserDiagnostics {
    fun describe(bootstrap: Map<String, Any>, readiness: Map<String, Any>, relay: Map<String, Long>, vpn: Map<String, String> = emptyMap()): String {
        val lines = mutableListOf("Последняя завершённая попытка в текущем процессе. Это не архив и не новая проверка сети.")
        fun enum(key: String, allowed: Set<String>) {
            val value = bootstrap[key]
            if (value is String && value in allowed) lines += "$key: $value"
        }
        enum("operation", setOf("UNKNOWN", "CHALLENGE", "CATALOG", "STATUS", "REFRESH", "REGISTER"))
        enum("io_stage", setOf("DIAL", "HANDSHAKE", "WRITE", "READ", "COMPLETE"))
        enum("first_close", setOf("NONE", "UNKNOWN", "RELAY_READ", "RELAY_WRITE", "PIPE_READ", "PIPE_WRITE", "CALLER_CANCEL", "CLEANUP"))
        enum("error_class", setOf("NONE", "EOF", "CANCELED", "DEADLINE", "OTHER"))
        for (key in listOf("handshake_pin_ok", "failed", "caller_canceled", "caller_deadline", "transport_canceled", "transport_deadline")) {
            val value = bootstrap[key]
            if (value is Boolean) lines += "$key: $value"
        }
        for (key in listOf("statsOk", "handshakePresent", "handshakeFresh", "vpnPresent", "dnsOk", "httpsOk", "httpStatusOk", "expectedExitOk", "ready", "deadline")) {
            val value = readiness[key]
            if (value is Boolean) lines += "$key: $value"
        }
        // Bounded readiness sub-cause: fixed enums/codes only, never raw payload or exception text.
        readiness["stage"]?.takeIf { it is String &&
            it in setOf("APPLY", "VPN_NETWORK", "DNS", "HTTPS", "HTTP_STATUS", "EXPECTED_EXIT", "WG_STATS", "READY") }
            ?.let { lines += "stage: $it" }
        readiness["exceptionClass"]?.takeIf { it is String &&
            it in setOf("NONE", "TIMEOUT", "DNS", "TLS", "IO", "SECURITY", "OTHER") }
            ?.let { lines += "exceptionClass: $it" }
        readiness["failureCode"]?.takeIf { it is String && it.matches(Regex("[A-Z][A-Z0-9_]{1,63}")) }
            ?.let { lines += "failureCode: $it" }
        readiness["elapsed_ms"]?.let { if (it is Long && it in 0..60_000) lines += "readiness_elapsed_ms: $it" }
        // local_udp_rx_* is the UPLINK counter (WG peer -> relay); already collected into
        // lastRelay, surfaced here display-only so uplink emission is distinguishable from a
        // lost downlink. No new counter, no behaviour change.
        for (key in listOf("config_worker_start_count", "config_worker_ready_count", "data_worker_start_count", "data_worker_ready_count", "local_udp_rx_packets", "local_udp_rx_bytes", "local_udp_write_packets", "local_udp_write_errors", "relay_rx_packets")) {
            relay[key]?.takeIf { it >= 0 }?.let { lines += "$key: $it" }
        }
        vpn["vpn_stage"]?.takeIf { it in setOf("PENDING", "HANDSHAKE", "AUTH", "CONFIG", "BRIDGE") }?.let { lines += "vpn_stage: $it" }
        vpn["vpn_error_class"]?.takeIf { it in setOf("NONE", "TIMEOUT", "CANCELED", "FAILED") }?.let { lines += "vpn_error_class: $it" }
        vpn["vpn_auth_code"]?.takeIf { it in setOf("NONE", "UNKNOWN", "AUTH_REQUIRED", "BAD_MESSAGE", "GRANT_REVOKED", "LEASE_CONFLICT", "CHALLENGE_EXPIRED", "LEASE_EXPIRED", "PROOF_INVALID", "TRUST_FAILED", "KEY_UNAVAILABLE", "TRANSPORT_CLOSED", "RETRY_EXHAUSTED") }?.let { lines += "vpn_auth_code: $it" }
        if (lines.size == 1) lines += "Данных ещё нет либо процесс приложения был перезапущен."
        lines += "Счётчики накопительные, не число одновременно работающих соединений. DNS_FAILED сам по себе не устанавливает причину сбоя."
        return lines.joinToString("\n")
    }
}
