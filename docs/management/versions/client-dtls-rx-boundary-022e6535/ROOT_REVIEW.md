# Root source review

Accepted source/offline scope: code022e6535, fullHEAD0e735935, base74db78c0. All10 changed file hashes and8 unchanged Git blobs matched canonical subtree import. Full101feature IDs retained.

Read all runtime delta, new Go tests and Kotlin consumer tests plus Pion receive analysis. RX helper retains original peer/unwrap/drop/pipe policy; TX returns underlying n/error; nil observer changes no IO/deadlines/close. Actual Allocate LocalAddr lookup is mobile-only, before wrapper. Bounded8 records/datagram and8record tail with independent counters; malformed metadata never affects forwarding. Existing seal captures snapshot, late=1 explicitly limits time interpretation. Only outer record headers, no payload/Finished inference. Go5 race PASS1.068s and finalbounds PASS1.046s; Kotlin2 PASS0.076s. No redundant rerun.

Consumer supports full76line single batch,314byte IO grammar, limits76/10s304/child; older stream caps unchanged. Multiple bursts may still truncate; missingFINISH means incomplete. Successful pipe write only asynchronous queue handoff; Pion epoch/replay/CID/decrypt/fragment/verify remains downstream. No proven defect or productPASS. New build/install/device NOT_TESTED until next actual run. Next: build native+Kotlin together, parity/install without data wipe, one coldprocess launch and endpoint-correlated ordinary server logs; no API probe needed.
