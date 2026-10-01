# Root review: mobile service dial timing

Accepted source b65be933351f973194b97954eb663bde0e5fc909; documented Laptop HEAD74db78c0da2f5b70dba577e572c98cbccb37b9ec, basec7feddd. Reviewed cumulative patch637e799a387ba1a6ada3ee56d8588620a890693684e443d1155dffdb705a71c9; all nine imported file SHA256 values match provenance.

Read runtime integration, bounded observer, first-write delegating wrapper, Kotlin consumer, and targeted tests. Scope is managedServiceEstablish only; nil observer preserves ordinary VPN callers. Context cancellation and timeout values, candidate order, credentials, dependencies and return values unchanged. Concurrency handled by atomic first-write claim and sealed mutex buffer; events after captured finish excluded. Successful dial timestamps precede one bounded stderr emission. That emission is before AUTH and contributes observer overhead; no claim of zero measurement effect. Hard-killed process or host rate limit can lose output; missing FINISH is incomplete evidence. First-write span includes Permission plus other WriteTo work, not pure Permission latency. No packet payload/address logging.

Accepted existing checks: seven targeted Go race tests PASS1.174s; three actual Kotlin consumer JUnit tests PASS0.072s. Tests not repeated. Publication fix and dependency blobs preserved. Source review only: native/APK/install/device for this change not yet tested. Existing device CATALOG_TIMEOUT remains FAIL; measurement is not a latency fix.

Monorepo client subtree import uses canonical publisher; Laptop donor repository unchanged. Current TEST gateway remains isolated bebec698 artifact, not rebuilt from cumulative monorepo. API phase flag prepared on TEST; restart confound retained.
