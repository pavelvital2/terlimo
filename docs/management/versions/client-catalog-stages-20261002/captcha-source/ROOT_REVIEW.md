# CAPTCHA source review

Accepted source candidate from Laptop 806d54a762848436108d645a63b7cc6fc63ccad7, base 5ab55149da7e53a030e4a08ce65183b5b89d70e2.

Root reviewed the complete ten-file patch and caller context. Catalogue CAPTCHA pause owns attempt/cycle/request, preserves stage and total remainders, rejects accept/advance while pending and fences expired/stale/cleared callbacks. Existing finite solver timeouts bound the pause. Manual Activity/JS/notification callbacks capture immutable owner and atomically consume only matching pending. Retiring Service scope cancellation prevents deferred cleanup from being discarded by actor close. Existing stage failure and Connected refresh handling remain in place. No Go/server changes.

Executor receipts: 13 catalogue + 3 manual-owner JVM tests PASS, Android fixtures compile PASS only. The two before failures used no-op API shims, not a pristine baseline or phone reproduction. Root did not rerun successful tests. Full previous passport preserved byte-equivalently except surrounding whitespace; no historical status upgraded.

Next: Android build with unchanged native792, preserve installed identity/rollback; narrow local WebView device checks and ordinary catalogue smoke. Device lifecycle/natural VK CAPTCHA are not established by source/JVM evidence. Installed ef84 remains the accepted baseline until new installation and verification receipts.
