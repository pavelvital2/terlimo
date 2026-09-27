# TERLIMO source backup

This private repository holds source snapshots for the TERLIMO gateway, server, and client applications. The sole branch is `Терлимо`. This is a source backup during TEST development, not a production release or a claim that all user flows have passed.

| Directory | Source snapshot |
| --- | --- |
| `gateway/` | TEST A gateway source, commit `59bf41c0dea9c486fec5540d0b6ef72dc686ce48`, tree `5f64a6dafee86ead036c39511e0c2793fda23223` |
| `server/` | TEST A backend source, commit `c526b242d17b059690472d3e079dd1b7f73153d0`, tree `ef33979387743b892fec63f0c408455d01465362` |
| `clients/android/` | Android accepted TEST source, commit `3eaea7121a917940ba1fa2183989023d4742f767`, tree `2aac64db5208c6ebbf56463286eae05822acc2c5` |

The snapshots omit deployment secrets, live database data, logs, device profiles, TEST mobile configuration, APKs, and other generated binaries. `clients/android/` retains its upstream license and attribution. Local build prerequisites and nonsecret configuration must be supplied separately; see component documentation. Later accepted changes should be committed here after the relevant TEST check.

## Snapshot limits

This backup is intentionally filtered. The original full Git tree IDs above identify the source revisions; the files committed here are a source-only selection, not byte-for-byte copies of those complete trees. `gateway/udp_listener_test.go` is restored unchanged after its inline literal was confirmed synthetic. `server/tools/gateway_integration_03_3.py` is restored with only its fixed TEST password at line 50 replaced by `<SET_TEST_GATEWAY_PASSWORD>` because the original may be operational. Thirteen marked-synthetic JSON fixtures and one unconfirmed fixture, plus live deployment templates, remain absent from the server export. Review and restore fixtures separately before claiming full standalone test or deployment reproducibility.

The Android `go_client` tests pass in this extracted layout; the gateway Go packages build and server Python files compile. These checks do not substitute for a complete product acceptance test. The TEST A user12 VPN and HTTPS exit path was accepted on 2026-09-27 from paired phone and server evidence; other scenarios remain open.
