# Trusted referral adapter SOURCE acceptance

Donor737fb6a3e2bddc8fc2864f7c460e390bd448c67c/base3ecc432 integrated over reminder fix91d4d1a. Nine source postimages match; reminder postimages preserved. Root reviewed actual internal authentication, verified identity, durable key replay/body conflict, shared mobile/canonical projection and corrected row-lock/initializer seams.

A1 closed: NO KEY UPDATE in trusted/current account, shared initializer and shared attachment permits reciprocal FK KEY SHARE while serializing same-account mutation. Unique referral_code assignment is separate and only for a missing code; ordinary update no longer upgrades the lock by listing that unique column. Existing advisory/account→benefit order retained. Actual PG barriers cover trusted/trusted, mobile/trusted, mobile/mobile with both own rows held; ready succeeds, concurrent uncommitted-pending remains transient without terminal mutations.

A2 closed: ready/code-bearing legacy accounts are not reinitialized against stale pre-epoch inviter snapshots. New key yields durable already_attributed; exact original key replays and actual owner/used flags remain.

Executor evidence: initial adapter10PASS, red reproduction2FAIL on donor4bb2b553, final correction7PASS3.80s plus one specifically affected initializer preservationPASS1.49s. Root verified artifact hashes/rawfinal logs/read actual tests; did not rerun suites. Intermediate test expectation failures were corrected to valid transient pending semantics, not concealed as product PASS.

SOURCE accepted only. TEST0040 deployment and real bot/site caller integration, verified mapping/checkout/trial writer transfer, legacy history/pending obligations remain outstanding. No production apply or full single-writer PASS. Public mobile/referral/registration wire and financial behavior unchanged.
