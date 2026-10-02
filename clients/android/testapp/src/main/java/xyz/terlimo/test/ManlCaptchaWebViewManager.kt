// Ported from WDTT Plus v20 (GPLv3), donor commit d450e132d2477a80d7079282c200ac3fb97ad0ae,
// donor file app/src/main/java/com/wdtt/plus/ManlCaptchaWebViewManager.kt (official manual
// CAPTCHA window + notification + foreground/background relaunch). Kept 1:1 except:
//  * package/imports;
//  * the Compose shell of ManlCaptchaActivity is rendered with the platform View layer
//    (testapp has no Compose) while every WebView setting, the interceptor/close JS, the
//    touch handling and Back semantics are retained; callbacks and cleanup are owner-bound;
//  * foreground bookkeeping uses AppForeground/CaptchaPendingPolicy (same decisions);
//  * the notification cancel action routes to the existing SessionService "cancel" intent
//    (same "disable the tunnel" effect as the donor TunnelManager.stop).
// License/attribution preserved; see the repository LICENSE (GPLv3).
package xyz.terlimo.test

import android.annotation.SuppressLint
import android.app.Activity
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.os.Bundle
import android.util.Log
import android.view.Gravity
import android.view.MotionEvent
import android.view.ViewGroup
import android.webkit.*
import android.widget.FrameLayout
import android.widget.ProgressBar
import androidx.core.app.NotificationCompat
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withTimeout
import java.util.UUID

object ManlCaptchaWebViewManager {
    private const val TAG = "ManlCaptchaWV"
    private const val CAPTCHA_TIMEOUT_MS = 180_000L

    val captchaMutex = Mutex()
    const val EXTRA_CAPTCHA_OWNER = "manualCaptchaOwner"
    private data class Request(val result: CompletableDeferred<Result<String>>, val intent: Intent)
    private data class Window(val owner: String, val activity: ManlCaptchaActivity)
    private val pending = ManualCaptchaPendingOwner<Request>()
    private var displayedRequest: ManualCaptchaPendingOwner.Pending<Request>? = null
    private var window: Window? = null

    val pendingOwner: String? get() = synchronized(pending) { pending.current()?.owner }
    val activeActivity: ManlCaptchaActivity? get() = synchronized(pending) { window?.activity }
    val pendingIntentToStart: Intent? get() = synchronized(pending) { pending.current()?.value?.intent }
    val isCaptchaPending: Boolean get() = synchronized(pending) { pending.current() != null }

    fun checkAndShowPendingCaptcha(context: Context) = synchronized(pending) {
        val current = pending.current() ?: return@synchronized
        if (CaptchaPendingPolicy.shouldRelaunchPending(true, window?.owner == current.owner)) {
            context.startActivity(current.value.intent)
        }
    }

    /** Notification commands are checked again when SessionService handles the queued Intent. */
    fun runIfPendingOwner(owner: String, action: () -> Unit): Boolean = synchronized(pending) {
        if (pending.current()?.owner != owner) return@synchronized false
        action()
        true
    }

    fun cancelCaptcha() = synchronized(pending) {
        val current = pending.current() ?: return@synchronized
        pending.consume(current.owner)?.value?.result
            ?.completeExceptionally(CancellationException("Cancelled by system"))
    }

    internal fun attachActivity(owner: String, activity: ManlCaptchaActivity): Boolean = synchronized(pending) {
        if (pending.current()?.owner != owner) return@synchronized false
        val previous = window
        window = Window(owner, activity)
        if (previous?.activity !== activity) previous?.activity?.finishForOwner(previous.owner)
        true
    }

    internal fun detachActivity(owner: String?, activity: ManlCaptchaActivity): Boolean = synchronized(pending) {
        if (window?.owner != owner || window?.activity !== activity) return@synchronized false
        window = null
        val currentOwner = pending.current()?.owner ?: displayedRequest?.owner
        currentOwner == null || currentOwner == owner
    }

    private const val NOTIFICATION_ID = 9001
    private const val CHANNEL_ID = "captcha_channel"

    private fun showCaptchaNotification(context: Context, owner: String, openIntent: Intent) {
        if (!CaptchaPendingPolicy.shouldNotify(AppForeground.isForeground)) return

        val notificationManager = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager

        if (android.os.Build.VERSION.SDK_INT >= android.os.Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                "Уведомления защиты (Капча)",
                NotificationManager.IMPORTANCE_HIGH
            )
            notificationManager.createNotificationChannel(channel)
        }

        // Unique immutable PendingIntents prevent an old notification from acquiring a new owner.
        val openPendingIntent = PendingIntent.getActivity(
            context, 0, openIntent, PendingIntent.FLAG_IMMUTABLE
        )

        val cancelIntent = Intent(context, CaptchaCancelReceiver::class.java).apply {
            action = "manual_captcha_cancel:$owner"
            putExtra(EXTRA_CAPTCHA_OWNER, owner)
        }
        val cancelPendingIntent = PendingIntent.getBroadcast(
            context, 1, cancelIntent, PendingIntent.FLAG_IMMUTABLE
        )

        val notification = NotificationCompat.Builder(context, CHANNEL_ID)
            .setSmallIcon(android.R.drawable.ic_dialog_alert)
            .setContentTitle("Требуется подтверждение капчи")
            .setContentText("ВК запросил проверку безопасности. Нажмите для решения.")
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setContentIntent(openPendingIntent)
            .setAutoCancel(true)
            .addAction(0, "Отменить и выключить", cancelPendingIntent)
            .build()

        notificationManager.notify(NOTIFICATION_ID, notification)
    }

    private fun clearCaptchaNotification(context: Context) {
        val notificationManager = context.getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        notificationManager.cancel(NOTIFICATION_ID)
    }

    suspend fun solveCaptchaAsync(context: Context, redirectUri: String, sessionToken: String): String {
        return captchaMutex.withLock {
            val owner = UUID.randomUUID().toString()
            val deferred = CompletableDeferred<Result<String>>()
            val intent = Intent(context, ManlCaptchaActivity::class.java).apply {
                action = "manual_captcha_open:$owner"
                putExtra("redirectUri", redirectUri)
                putExtra(EXTRA_CAPTCHA_OWNER, owner)
                addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_EXCLUDE_FROM_RECENTS)
            }
            val request = ManualCaptchaPendingOwner.Pending(owner, Request(deferred, intent))
            try {
                synchronized(pending) {
                    pending.replace(request)?.value?.result?.cancel()
                    displayedRequest = request
                    showCaptchaNotification(context, owner, intent)
                    if (CaptchaPendingPolicy.shouldStartActivity(AppForeground.isForeground)) {
                        context.startActivity(intent)
                    }
                }
                withTimeout(CAPTCHA_TIMEOUT_MS) { deferred.await().getOrThrow() }
            } finally {
                synchronized(pending) {
                    pending.consume(owner)
                    if (displayedRequest?.owner == owner) {
                        displayedRequest = null
                        clearCaptchaNotification(context)
                        val currentWindow = window
                        if (currentWindow?.owner == owner) {
                            // The queued finish also checks its bound owner after onNewIntent.
                            currentWindow.activity.finishForOwner(owner)
                        }
                    }
                }
            }
        }
    }

    fun notifyResult(owner: String, result: Result<String>): Boolean = synchronized(pending) {
        val request = pending.consume(owner) ?: return@synchronized false
        request.value.result.complete(result)
    }
}

/** View-layer port of the donor Compose window; WebView logic/JS are donor-exact. */
class ManlCaptchaActivity : Activity() {
    override fun attachBaseContext(newBase: android.content.Context) {
        super.attachBaseContext(AppTheme.wrap(newBase))
    }

    private var browser: WebView? = null
    private var loading: ProgressBar? = null
    private var boundOwner: String? = null

    private val interceptorJSCode = """
        (function() {
            if (window.__wdtt_interceptor_installed) return;
            window.__wdtt_interceptor_installed = true;

            function getParam(body, key) {
                try {
                    if (!body) return '';
                    if (typeof body === 'string') return new URLSearchParams(body).get(key) || '';
                    if (body instanceof URLSearchParams) return body.get(key) || '';
                    if (body instanceof FormData) return body.get(key) || '';
                } catch(e) {}
                return '';
            }

            function reportPayload(body) {
                try {
                    const browserFp = getParam(body, 'browser_fp');
                    const adFp = getParam(body, 'adFp');
                    const debugInfo = getParam(body, 'debug_info');
                    if (browserFp || adFp || debugInfo) {
                        window.WdttCaptcha.onCheckPayload(
                            browserFp ? browserFp.length : 0,
                            adFp ? adFp.length : 0,
                            debugInfo ? debugInfo.length : 0
                        );
                    }
                } catch(e) {}
            }

            const origFetch = window.fetch;
            window.fetch = async function() {
                const args = arguments;
                const url = args[0] || '';
                if (typeof url === 'string' && url.includes('captchaNotRobot.check')) {
                    reportPayload(args[1] && args[1].body);
                    const response = await origFetch.apply(this, args);
                    const clone = response.clone();
                    try {
                        const data = await clone.json();
                        if (data.response && data.response.success_token) {
                            window.WdttCaptcha.onSuccess(data.response.success_token);
                        } else if (data.response && data.response.status) {
                            window.WdttCaptcha.onCheckStatus(
                                data.response.status || '',
                                data.response.show_captcha_type || ''
                            );
                        } else if (data.error) {
                            window.WdttCaptcha.onError(JSON.stringify(data.error));
                        }
                    } catch(e) {}
                    return response;
                }
                return origFetch.apply(this, args);
            };
            
            const origXHROpen = XMLHttpRequest.prototype.open;
            const origXHRSend = XMLHttpRequest.prototype.send;
            XMLHttpRequest.prototype.open = function(method, url) {
                this._wdtt_url = url;
                return origXHROpen.apply(this, arguments);
            };
            XMLHttpRequest.prototype.send = function() {
                const xhr = this;
                if (xhr._wdtt_url && xhr._wdtt_url.includes('captchaNotRobot.check')) {
                    reportPayload(arguments[0]);
                    xhr.addEventListener('load', function() {
                        try {
                            const data = JSON.parse(xhr.responseText);
                            if (data.response && data.response.success_token) {
                                window.WdttCaptcha.onSuccess(data.response.success_token);
                            } else if (data.response && data.response.status) {
                                window.WdttCaptcha.onCheckStatus(
                                    data.response.status || '',
                                    data.response.show_captcha_type || ''
                                );
                            } else if (data.error) {
                                window.WdttCaptcha.onError(JSON.stringify(data.error));
                            }
                        } catch(e) {}
                    });
                }
                return origXHRSend.apply(this, arguments);
            };
        })();
    """.trimIndent()

    private val closeBridgeJSCode = """
        (function() {
            if (window.__wdtt_close_bridge_installed) return;
            window.__wdtt_close_bridge_installed = true;
            document.addEventListener('click', function(e) {
                if (e.target.closest('.vkc__ModalCardBase-module__dismiss')) {
                    window.WdttCaptcha.onCancel();
                }
            });
        })();
    """.trimIndent()

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        setTheme(AppTheme.platformTheme())
        super.onCreate(savedInstanceState)
        if (!showRequest(intent)) finish()
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        // A stale notification must neither reload a redirect nor close the current window.
        val owner = intent.getStringExtra(ManlCaptchaWebViewManager.EXTRA_CAPTCHA_OWNER) ?: return
        if (ManlCaptchaWebViewManager.pendingOwner != owner) return
        if (boundOwner == owner) return
        showRequest(intent)
    }

    internal fun finishForOwner(owner: String) {
        runOnUiThread { if (boundOwner == owner) finish() }
    }

    @SuppressLint("SetJavaScriptEnabled")
    private fun showRequest(requestIntent: Intent): Boolean {
        val owner = requestIntent.getStringExtra(ManlCaptchaWebViewManager.EXTRA_CAPTCHA_OWNER) ?: return false
        val redirectUri = requestIntent.getStringExtra("redirectUri") ?: return false
        if (!ManlCaptchaWebViewManager.attachActivity(owner, this)) return false
        destroyBrowser()
        boundOwner = owner
        setIntent(requestIntent)
        AppForeground.isForeground = true

        val root = FrameLayout(this)
        val web = WebView(this)
        browser = web
        val progress = ProgressBar(this).apply { isIndeterminate = true }
        loading = progress
        root.addView(web, FrameLayout.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT))
        root.addView(progress, FrameLayout.LayoutParams(
            ViewGroup.LayoutParams.WRAP_CONTENT, ViewGroup.LayoutParams.WRAP_CONTENT, Gravity.CENTER))
        setContentView(root)

        web.setBackgroundColor(android.graphics.Color.TRANSPARENT)
        web.isNestedScrollingEnabled = false
        web.setOnTouchListener { v, event ->
            when (event.actionMasked) {
                MotionEvent.ACTION_DOWN,
                MotionEvent.ACTION_MOVE,
                MotionEvent.ACTION_POINTER_DOWN -> {
                    v.parent?.requestDisallowInterceptTouchEvent(true)
                }
                MotionEvent.ACTION_UP,
                MotionEvent.ACTION_CANCEL -> {
                    v.parent?.requestDisallowInterceptTouchEvent(false)
                }
            }
            false
        }
        web.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            databaseEnabled = true
            mediaPlaybackRequiresUserGesture = false
            loadWithOverviewMode = true
            useWideViewPort = true
            blockNetworkLoads = false
            cacheMode = WebSettings.LOAD_DEFAULT // Включаем кэш для моментальной загрузки!
            userAgentString = WebSettings.getDefaultUserAgent(this@ManlCaptchaActivity)
        }

        web.addJavascriptInterface(object {
            @JavascriptInterface
            fun onSuccess(token: String) {
                Log.d("ManlCaptchaWV", "Token received")
                if (ManlCaptchaWebViewManager.notifyResult(owner, Result.success(token))) finishForOwner(owner)
            }
            @JavascriptInterface
            fun onError(err: String) {
                Log.e("ManlCaptchaWV", "VK captcha check failed")
                if (ManlCaptchaWebViewManager.notifyResult(owner,
                        Result.failure(Exception("VK Captcha error: $err")))) finishForOwner(owner)
            }
            @JavascriptInterface
            fun onCheckStatus(status: String, showType: String) {
                Log.i("ManlCaptchaWV", "captcha check status=$status show_type=$showType")
            }
            @JavascriptInterface
            fun onCheckPayload(browserFpLen: Int, adFpLen: Int, debugInfoLen: Int) {
                Log.i("ManlCaptchaWV", "check payload browser_fp_len=$browserFpLen adFp_len=$adFpLen debug_len=$debugInfoLen")
            }
            @JavascriptInterface
            fun onCancel() {
                Log.d("ManlCaptchaWV", "User closed VK captcha")
                if (ManlCaptchaWebViewManager.notifyResult(owner,
                        Result.failure(Exception("Cancelled by user")))) finishForOwner(owner)
            }
        }, "WdttCaptcha")

        web.webViewClient = object : WebViewClient() {
            override fun onPageStarted(view: WebView?, url: String?, favicon: android.graphics.Bitmap?) {
                super.onPageStarted(view, url, favicon)
                view?.evaluateJavascript(interceptorJSCode, null)
            }
            override fun onPageFinished(view: WebView?, url: String?) {
                super.onPageFinished(view, url)
                view?.evaluateJavascript(interceptorJSCode, null)
                view?.evaluateJavascript(closeBridgeJSCode, null)
                loading?.visibility = android.view.View.GONE
            }
        }
        web.webChromeClient = WebChromeClient()
        web.loadUrl(redirectUri)
        return true
    }

    private fun destroyBrowser() {
        browser?.apply {
            stopLoading()
            try { removeJavascriptInterface("WdttCaptcha") } catch (_: Exception) {}
            destroy()
        }
        browser = null
        loading = null
    }

    override fun onDestroy() {
        super.onDestroy()
        if (ManlCaptchaWebViewManager.detachActivity(boundOwner, this)) {
            AppForeground.isForeground = false
        }
        destroyBrowser()
        // Мы НЕ отправляем ошибку здесь! 
        // Если юзер смахнул окно (нажал назад), капча останется висеть в памяти (через пуш).
        // Ошибка или Успех отправляются только по явным действиям (крестик, решение, или таймаут 180 секунд).
    }
}

class CaptchaCancelReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val owner = intent.getStringExtra(ManlCaptchaWebViewManager.EXTRA_CAPTCHA_OWNER) ?: return
        ManlCaptchaWebViewManager.runIfPendingOwner(owner) {
            val stop = Intent(context, SessionService::class.java).setAction("cancel")
                .putExtra(ManlCaptchaWebViewManager.EXTRA_CAPTCHA_OWNER, owner)
            try {
                context.startForegroundService(stop)
            } catch (_: Exception) {
                runCatching { context.startService(stop) }
            }
        }
    }
}
