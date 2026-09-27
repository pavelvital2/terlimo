# Snapshot provenance — 2026-09-27

| Component | Original source commit | Original full tree | Local export |
| --- | --- | --- | --- |
| gateway | `59bf41c0dea9c486fec5540d0b6ef72dc686ce48` | `5f64a6dafee86ead036c39511e0c2793fda23223` | Filtered source tar SHA-256 `60207be3b546f365fab65b74587360ea6818caffd7e0d3e8ef058bd68b8df93d` |
| server | `c526b242d17b059690472d3e079dd1b7f73153d0` | `ef33979387743b892fec63f0c408455d01465362` | Filtered source tar SHA-256 `ebb71cd850389c20aaa06a6b2b033d410b805955c7db2946af6fcfdb25b9f7ee` |
| clients/android | `3eaea7121a917940ba1fa2183989023d4742f767` | `2aac64db5208c6ebbf56463286eae05822acc2c5` | Selected `app/`, `testapp/`, `go_client/`, Gradle, build tools, docs fixtures from Git archive; private `test-mobile.json` omitted. |

Gateway and server sources were exported from TEST A's persistent Git object databases. Android sources came from the accepted Laptop worktree commit rather than its dirty local TEST overlay. This repository has no live secrets, DB, profile, logs or compiled application artifacts by design. A local source backup is not proof of TEST B parity, production readiness or payment acceptance.
