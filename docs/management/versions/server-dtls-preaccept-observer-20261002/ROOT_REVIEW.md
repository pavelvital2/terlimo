# Root source review — DTLS preaccept observer

Accepted source/offline only. Exact isolated build source: 440579f235a5fd605c396c3bae41c22e2104a25f on base d847a41168c54f413e0a961e1ce198db116650b9. Canonical gateway file hashes match the three-file reviewed patch. The cumulative canonical gateway is not the isolated build input.

OFF observer is nil; ON uses a fixed 8-peer/64-event ledger. Review covered receive timestamps, queue admission, exactly-once buffer writes, Accept dequeue, enum errors, connection identity, mutex ordering, bounded output, and lifecycle. Existing admission, packet delivery, routing, product deadlines and retransmission rules are unchanged. Default flag absent/OFF. No raw endpoint, key or payload is retained.

Five targeted Go1.26.5 race tests passed in the executor report, including OFF/ON equivalence and trigger delivery before/after Accept. Existing passing suites were not rerun. Full 101-feature passport is preserved.

Limits: ReadBatch timestamp is shared userspace return time, not kernel arrival. No receive is NOT_OBSERVED; incomplete/interim capture cannot prove absence. Listener END is a defined capture cutoff; accepted connections may outlive it. Event/peer caps and enabled observer overhead remain measurement limitations. This is instrumentation, not a demonstrated product repair. No build, install, live flags or phone run accepted here.
