"""Public recovery page in the existing server; no session, DB, resolver or rights."""
import base64
import hashlib
from collections.abc import Mapping
from html import escape

from aiohttp import web

from .recovery_code import PublicRecoveryCode, RecoveryUnavailable

TEXTS = {
    "title": "Восстановить подключение",
    "instruction": "Скопируйте код и вернитесь в TERLIMO: Настройки → Восстановить подключение → вставьте код.",
    "rights": "Покупка и повторная регистрация не нужны. Код меняет параметры подключения, а не аккаунт или права доступа.",
    "unavailable": "Код восстановления сейчас недоступен. Попробуйте позже.",
    "copy": "Скопировать",
    "copied": "Код скопирован. Вернитесь в приложение.",
    "copy_failed": "Не удалось скопировать автоматически. Выделите код и скопируйте его вручную.",
    "code_label": "Код восстановления",
}
COPY_SCRIPT = """const code = document.getElementById('recovery-code');
const button = document.getElementById('copy-code');
const status = document.getElementById('copy-status');
if (code && button && status) {
  button.disabled = false;
  button.addEventListener('click', async () => {
    try {
      if (!navigator.clipboard) throw new Error('clipboard unavailable');
      await navigator.clipboard.writeText(code.value);
      status.textContent = status.dataset.copied;
    } catch (_) {
      code.focus(); code.select();
      status.textContent = status.dataset.failed;
    }
  });
}
"""
SCRIPT_HASH = base64.b64encode(hashlib.sha256(COPY_SCRIPT.encode()).digest()).decode()


def recovery_page_response(loader: PublicRecoveryCode, texts: Mapping[str, str] = TEXTS,
                           *, language: str = "ru") -> web.Response:
    try:
        code = loader.get()
        available = True
    except RecoveryUnavailable:
        code = ""
        available = False
    t = {k: escape(v, quote=True) for k, v in texts.items()}
    content = (f'<label for="recovery-code">{t["code_label"]}</label>'
               f'<textarea id="recovery-code" readonly rows="8" spellcheck="false">{escape(code)}</textarea>'
               f'<button id="copy-code" type="button" disabled>{t["copy"]}</button>'
               f'<p id="copy-status" role="status" aria-live="polite" data-copied="{t["copied"]}" '
               f'data-failed="{t["copy_failed"]}"></p>') if available else f'<p role="status">{t["unavailable"]}</p>'
    body = (f'<!doctype html><html lang="{escape(language, quote=True)}"><head><meta charset="utf-8">'
            '<meta name="viewport" content="width=device-width,initial-scale=1">'
            f'<title>TERLIMO — {t["title"]}</title>'
            '<style>body{font:18px system-ui,sans-serif;max-width:42rem;margin:2rem auto;padding:0 1rem;line-height:1.5}'
            'textarea{box-sizing:border-box;width:100%;font:14px monospace;overflow-wrap:anywhere;margin:.5rem 0}'
            'button{font:inherit;padding:.65rem 1rem;min-height:44px}label{display:block}</style></head>'
            f'<body><main><h1>{t["title"]}</h1><p>{t["instruction"]}</p>{content}<p>{t["rights"]}</p>'
            f'<noscript><p>{t["copy_failed"]}</p></noscript></main><script>{COPY_SCRIPT}</script></body></html>')
    return web.Response(text=body, content_type="text/html", status=200 if available else 503, headers={
        "Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff",
        "Content-Security-Policy": f"default-src 'none'; script-src 'sha256-{SCRIPT_HASH}'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'",
    })


def register_public_recovery_page(app: web.Application, loader: PublicRecoveryCode) -> None:
    async def page(_request: web.Request) -> web.Response:
        return recovery_page_response(loader)
    app.router.add_get("/api/public/recovery", page)
