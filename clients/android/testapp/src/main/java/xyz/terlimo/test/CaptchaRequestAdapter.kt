package xyz.terlimo.test

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.TimeoutCancellationException

/** One CAPTCHA solve backend; production delegates to the ported donor v20 managers. */
internal interface CaptchaSolver {
    suspend fun solveAuto(redirectUri: String, sessionToken: String, onStep: (String) -> Unit): String
    suspend fun solveManual(redirectUri: String, sessionToken: String): String
}

/**
 * Official v20 dispatcher semantics (donor TunnelManager.handleCaptchaSolve:
 * `mode auto` → one bounded auto-WebView; `mode manual` → the manual window; everything else
 * (selected) follows the selected solve method: auto → two bounded auto attempts with an
 * honest manual fallback, otherwise the manual window. Error mapping mirrors the donor:
 * timeout → error:timeout, cancellation → error:cancelled, otherwise error:<message>.
 */
internal class CaptchaRequestAdapter(
    private val solver: CaptchaSolver,
    private val selectedMethod: () -> String = { DEFAULT_SELECTED_METHOD },
    private val log: (String) -> Unit = {},
) {
    suspend fun solve(mode: String, redirectUri: String, sessionToken: String, onStep: (String) -> Unit = {}): String {
        return when (mode.lowercase()) {
            MODE_AUTO -> solver.solveAuto(redirectUri, sessionToken, onStep)
            MODE_MANUAL -> solver.solveManual(redirectUri, sessionToken)
            else -> if (selectedMethod().lowercase() == MODE_AUTO) {
                solveSelectedAuto(redirectUri, sessionToken, onStep)
            } else {
                solver.solveManual(redirectUri, sessionToken)
            }
        }
    }

    private suspend fun solveSelectedAuto(redirectUri: String, sessionToken: String, onStep: (String) -> Unit): String {
        for (attempt in 1..SELECTED_AUTO_ATTEMPTS) {
            log("авто-WebView $attempt/$SELECTED_AUTO_ATTEMPTS, до 18 с")
            try {
                return solver.solveAuto(redirectUri, sessionToken, onStep)
            } catch (e: TimeoutCancellationException) {
                if (attempt == SELECTED_AUTO_ATTEMPTS) {
                    log("две авто-попытки не ответили, открыт ручной WebView")
                    return solver.solveManual(redirectUri, sessionToken)
                }
            } catch (e: IllegalStateException) {
                if (e.message == ERROR_SLIDER_DETECTED) {
                    log("обнаружен слайдер, открыт ручной WebView")
                    return solver.solveManual(redirectUri, sessionToken)
                }
                throw e
            }
        }
        return solver.solveManual(redirectUri, sessionToken)
    }

    fun errorValue(t: Throwable): String = when (t) {
        is TimeoutCancellationException -> "error:timeout"
        is CancellationException -> "error:cancelled"
        is IllegalStateException -> "error:${t.message ?: WV_STATE_ERROR}"
        else -> "error:${t.message ?: t::class.simpleName ?: UNKNOWN_ERROR}"
    }

    companion object {
        const val MODE_AUTO = "auto"
        const val MODE_MANUAL = "manual"
        const val MODE_SELECTED = "selected"
        const val DEFAULT_SELECTED_METHOD = "auto"
        const val SELECTED_AUTO_ATTEMPTS = 2
        const val ERROR_SLIDER_DETECTED = "slider_detected"
        const val WV_STATE_ERROR = "WV state error"
        const val UNKNOWN_ERROR = "unknown"
    }
}

/**
 * TERLIMO safety adaptation: the challenge window may only load the VK CAPTCHA origins.
 * A refused URL is localized to its own request (error:origin_invalid); the attempt and
 * every other channel stay untouched.
 */
internal object CaptchaOrigins {
    private val HOSTS = listOf("vk.com", "vk.ru", "vk.me", "vkuseraudio.net", "vkuser.net", "userapi.com", "vk-cdn.net")

    fun allowed(value: String): Boolean = runCatching {
        val uri = java.net.URI(value)
        val host = uri.host?.lowercase() ?: return@runCatching false
        uri.scheme == "https" && uri.userInfo == null && (uri.port == -1 || uri.port == 443) &&
            HOSTS.any { host == it || host.endsWith(".$it") }
    }.getOrDefault(false)
}
