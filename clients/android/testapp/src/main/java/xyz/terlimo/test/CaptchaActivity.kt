package xyz.terlimo.test

import android.app.Activity
import android.graphics.Bitmap
import android.net.Uri
import android.os.*
import android.view.WindowManager
import android.webkit.*
import android.widget.*
import java.io.ByteArrayInputStream

/** Bounded visible v17 CAPTCHA fallback; only success token crosses back into Go. */
class CaptchaActivity : Activity() {
    private var browser: WebView? = null
    private var prompt: CaptchaPrompt? = null
    private val main = Handler(Looper.getMainLooper())
    private var completed = false
    private val checkActive = object : Runnable {
        override fun run() {
            val current = prompt
            if (current == null || SessionService.captcha !== current) { finish(); return }
            if (SystemClock.elapsedRealtime() >= current.deadlineElapsed) { complete("error:timeout"); return }
            main.postDelayed(this, 250)
        }
    }
    @Suppress("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val current = SessionService.captcha ?: return finish()
        prompt = current
        if (!allowedUri(current.url)) return complete("error:cancelled")
        val layout = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        layout.addView(Button(this).apply { text = "Отменить CAPTCHA"; setOnClickListener { complete("error:cancelled") } })
        WebView.setWebContentsDebuggingEnabled(false)
        val web = WebView(this)
        browser = web
        web.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            allowFileAccess = false
            allowContentAccess = false
            mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
            cacheMode = WebSettings.LOAD_NO_CACHE
            saveFormData = false
        }
        web.importantForAutofill = android.view.View.IMPORTANT_FOR_AUTOFILL_NO_EXCLUDE_DESCENDANTS
        web.addJavascriptInterface(object {
            @JavascriptInterface fun success(token: String) {
                if (token.length in 1..16_384 && !token.startsWith("error:")) main.post { complete(token) }
            }
            @JavascriptInterface fun cancel() { main.post { complete("error:cancelled") } }
        }, "TerlimoCaptcha")
        web.webViewClient = object : WebViewClient() {
            override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean =
                !allowedUri(request.url.toString())
            override fun shouldInterceptRequest(view: WebView, request: WebResourceRequest): WebResourceResponse? {
                return if (allowedUri(request.url.toString())) null
                else WebResourceResponse("text/plain", "UTF-8", 403, "Blocked", emptyMap(), ByteArrayInputStream(ByteArray(0)))
            }
            override fun onReceivedSslError(view: WebView, handler: SslErrorHandler, error: android.net.http.SslError) {
                handler.cancel(); complete("error:cancelled")
            }
            override fun onPageStarted(view: WebView, url: String?, favicon: Bitmap?) {
                if (url == null || !allowedUri(url)) { view.stopLoading(); complete("error:cancelled"); return }
                view.evaluateJavascript(INTERCEPTOR, null)
            }
            override fun onPageFinished(view: WebView, url: String?) {
                if (url != null && allowedUri(url)) view.evaluateJavascript(INTERCEPTOR, null)
            }
        }
        layout.addView(web, LinearLayout.LayoutParams(-1, 0, 1f))
        setContentView(layout)
        web.loadUrl(current.url)
        main.post(checkActive)
    }
    private fun complete(value: String) {
        if (!completed) { completed = true; prompt?.complete?.invoke(value) }
        finish()
    }
    @Deprecated("Back cancels this attempt's CAPTCHA")
    override fun onBackPressed() { complete("error:cancelled") }
    override fun onDestroy() {
        main.removeCallbacksAndMessages(null)
        browser?.apply { stopLoading(); removeJavascriptInterface("TerlimoCaptcha"); clearCache(true); destroy() }
        browser = null
        CookieManager.getInstance().removeAllCookies(null)
        WebStorage.getInstance().deleteAllData()
        if (!completed && !isChangingConfigurations) prompt?.complete?.invoke("error:cancelled")
        super.onDestroy()
    }
    companion object {
        internal fun allowedUri(value: String): Boolean = runCatching {
            val uri = Uri.parse(value)
            val host = uri.host?.lowercase() ?: return@runCatching false
            uri.scheme == "https" && uri.userInfo == null && (uri.port == -1 || uri.port == 443) &&
                listOf("vk.com", "vk.ru", "vk.me", "vkuseraudio.net", "vkuser.net", "userapi.com", "vk-cdn.net").any {
                    host == it || host.endsWith(".$it")
                }
        }.getOrDefault(false)
        // Scoped adaptation of upstream ManlCaptchaWebViewManager's v17 fetch/XHR success interception.
        private val INTERCEPTOR = """
            (function() {
              if(window.__terlimoCaptcha) return; window.__terlimoCaptcha=true;
              function result(data) {
                if(data && data.response && data.response.success_token)
                  window.TerlimoCaptcha.success(String(data.response.success_token));
              }
              var fetchOriginal=window.fetch;
              window.fetch=async function() {
                var args=arguments, response=await fetchOriginal.apply(this,args);
                var url=typeof args[0]==='string'?args[0]:(args[0]&&args[0].url)||'';
                if(url.indexOf('captchaNotRobot.check')>=0) {
                  try { result(await response.clone().json()); } catch(e) {}
                }
                return response;
              };
              var openOriginal=XMLHttpRequest.prototype.open, sendOriginal=XMLHttpRequest.prototype.send;
              XMLHttpRequest.prototype.open=function(method,url) {
                this.__terlimoUrl=String(url); return openOriginal.apply(this,arguments);
              };
              XMLHttpRequest.prototype.send=function() {
                if((this.__terlimoUrl||'').indexOf('captchaNotRobot.check')>=0)
                  this.addEventListener('load',function(){try { result(JSON.parse(this.responseText)); } catch(e) {}});
                return sendOriginal.apply(this,arguments);
              };
              document.addEventListener('click',function(e) {
                if(e.target.closest('.vkc__ModalCardBase-module__dismiss')) window.TerlimoCaptcha.cancel();
              });
            })();
        """.trimIndent()
    }
}
