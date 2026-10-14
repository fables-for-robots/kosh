# Ledger of the build

The controller's record of the subagent-driven build of the metrics feature: for each task its commits, the review, the rulings, and what was parked. The rulings are the places to look when something here turns out wrong. (The working ledger, with the paths of the review packages, lives in the git-ignored SDD workspace; this is its copy, with the paths dropped.)


Branch `metrics` on main `ff4e8a0` (v0.3.0). Spec and plan committed as the first commit.

## Pre-flight scan

| Pair / task | Produces vs consumes | Found |
|---|---|---|
| T1 → T2 | `render.Usage` ← `reading` values | agree |
| T1 → T3 | `Metered.Meter(row, obj, usage map[string]Usage)`, `MeterContainers`, `StripMetrics` ← typedWatch/containersWatch | agree |
| T2 → T3 | `acquireMetrics/releaseMetrics/current/subscribe`, `metricsServed` ← watches, usageWatch | agree |
| T3 → T4 | `ClusterUsage`, `UsageWatch`, `kubetest.SetUsage` ← root/header | agree |
| T3 → T5 | columns present iff served ← e2e assertions | agree |
| T1 alone | positions table vs k9s order | checked against k9s 8f29fba pod.go/node.go/container.go |
| T3 alone | nodes cache held by usageWatch vs informer idle semantics (spec §4.5) | ruled: held for the session, released at Close (spec says so) |

Ruling: the spec was not put to the user for review before the plan; the user said "do the metrics" after choosing the design in chat (all of k9s; columns only while served) — proceeding; cost if wrong: rework of whatever they would have changed, visible in the PR.

## Task 1
BASE 051a2c5. Implementer dispatched (opus).
Implementer f0cb7c6 DONE_WITH_CONCERNS (minor; all consistent with the spec). Reviewer dispatched (opus) on its review package.
Review : spec ✅, quality approved; 5 minors. Ruling: fix 1 (ephemeral containers in the pod sum — spec §5 "the containers the pod's spec has" includes them), 2 (metricSetPerc via metricPerc), 3 (doc: Meter takes the full row) — cost if wrong: none beyond the round; park node branch cosmetics and the test's mutation check (the KeepsCells test covers it). Fix round 1 dispatched to the implementer.
Fix round 1: 030051b. Scoped re-review dispatched (sonnet) on its review package.

## Task 2
BASE 030051b. Implementer dispatched (opus) in parallel with the Task 1 re-review (different packages; a reviewer is read-only).
Re-review fix round 1: all three addressed, no new defect.
Task 1: complete (f0cb7c6, 030051b).
Implementer 35c7f83 DONE_WITH_CONCERNS (fake needs list kinds: newDynamicFake; timing bounds loosened; two small extras). Reviewer dispatched (opus) on its review package.
Review : spec ✅, quality approved; 5 minors. Ruling: fix 1 (ctx.Err at top of list), 2 (Close test asserts pollers cleared), 3 (newDynamicFake comment), 4 (quantity doc); park 5 (the "at once" subtest strength) — cost if wrong: a start that lists twice would go unnoticed by that test, caught by the lists count in the interval test. Fix round 1 dispatched.
Fix round 1: 023762d. Scoped re-review dispatched (sonnet). 

## Task 3
BASE 023762d. Implementer dispatched (opus) in parallel with the Task 2 re-review.
Re-review fix round 1: all four addressed, no new defect.
Task 2: complete (35c7f83, 023762d).
Implementer 08e58eb DONE_WITH_CONCERNS (changes channel instead of mergeChanged; usage watch non-nil when not served; poller acquired lazily on Current — flagged to the reviewer). Reviewer dispatched (opus) on its review package.
Review : spec ❌ (Important: first Changed up to a poll late for a second session; subscribe-after-list race), quality approved; minors. Ruling: fix the Important and minors 2–6; park the 40-poll bound and the small duplication. Fix round 1 dispatched to the implementer.
Fix round 1: d8bdb76 (signal at open and after subscribing; goUntil counts goroutines in c.wg; tick follows; envtest asserts). Scoped re-review dispatched (opus: the fix touches the lifecycle).

## Task 4
BASE d8bdb76. Implementer dispatched (opus) in parallel with the Task 3 re-review (tui only).
Re-review fix round 1: all six addressed, no new defect; two nits (a signal after a context-driven Close, harmless; the end-of-open notify has no test of its own when not served — parked, the final review may ask for it). 
Task 3: complete (08e58eb, d8bdb76).
Implementer d3eba20 DONE_WITH_CONCERNS: (1) no shutdown hook in the root — the wait on Changed is bounded at 1 min, as table waits are bounded at 2 s: ruled acceptable; (2) node cache signals clear the arrow between polls — ruled a kube defect: Task 3 implementer asked (fix round 2) to signal on polls only; (3) arrow vs the reading just before: as the brief; (4) red shade: fine. Reviewer dispatched (opus) on its review package.
Review : spec ✅, quality approved; minors. Ruling: fix 1 (arrow compares with the last ok reading across n/a — the more useful reading of "previous ok reading"), 3 (bold arrows, as k9s), test timer as a var; park usageCell signature. Fix round 1 dispatched.

## Task 5
BASE d3eba20 (plus the kube usage fix and the tui fix in flight; Task 5 touches scripts/, e2e/, README.md, charts values comment, the stage-1 spec). Implementer dispatched (opus).
Task 3 fix round 2: 3c11e4f (usage watch signals on polls, open, served change, and once at the nodes cache sync — ruled: keep the sync signal; it cannot clear an arrow). Scoped re-review dispatched (sonnet).
Re-review fix round 2: addressed; synced() sound; nit: the no-signal check in the new test is not strictly deterministic (an eventually() before it gives a forwarded signal time) — parked.
Task 3: complete (08e58eb, d8bdb76, 3c11e4f).
Task 4 fix round 1: 2eca4f9. Scoped re-review dispatched (sonnet).
Re-review fix round 1: all three addressed; nit: the timeout test shows the frame unchanged rather than a timeout fired — parked.
Task 4: complete (d3eba20, 2eca4f9).
Implementer 90338bf, c52f9dc DONE: e2e 48 pass on the box (416 s); slow test pass (600 s; columns go/return in ~5 min); chart script pass; box clean except ~/.claude.json rewritten by one of the user's own claude processes there (not ours). Additions: KOSH_E2E_RUN filter, 40-min deadline with SLOW, header lines checked in the slow test. Reviewer dispatched (opus) on its review package.
Integration gate: cli TestServeVanishedClientLeavesNothing failed on the branch head (waitUsage goroutine outliving its session by up to a minute). Ruling: fixed by me, 94a9d59 → amended (the watch closes Changed at Close; the root ends its wait; timer removed) — cost if wrong: the final review sees it. Task 5 review: spec ✅, quality needs work (Important: the slow test re-applies the stock manifest without --kubelet-insecure-tls, rollout stalls, old pod serves; minors on README wording, manifest checksum, values comment). Fix round 1 dispatched to the Task 5 implementer with a re-run of the slow test on the box.
Task 5 fix round 1: ded1562 (APIService saved and re-created alone; Deployment generation/args/rollout asserted; slow test re-run on the box: PASS 600 s; README/script/values minors). Scoped re-review dispatched (sonnet). Final whole-branch review dispatched (opus) on its review package.
Re-review Task 5 fix round 1: all five addressed; no new defect.
Task 5: complete (90338bf, c52f9dc, ded1562).

## Final review (final-review.md)
Verdict: not ready. C1 (Critical): ClusterUsage on an ended session → notify on the channel my 9d638b8 closes → panic (the Task 3 re-review nit I parked, made fatal by my fix). I1 (Important): nodes informer held for every session; README silent on nodes RBAC; view role → n/a forever. Minors M1–M6. Ruling: one fix dispatch (Task 3 implementer) for C1, I1 (code + README), M2, M3, M5; park M1 (poller restart on a namespace switch → brief n/a; a grace period would fix it; put to the user), M4 (test-only race in the fake), M6 (80-column layout: for the user). Suite on ded1562: all green with -race and envtest.
Final fix round: ece6edc (C1 notify/close under one lock + regression test; I1 nodes cache held only with the poller, WARN once, README RBAC; M3 AfterFunc always; M2 README), 039f21e (M5 TestSortByCPU). Scoped re-review dispatched (opus: C1 is a lifecycle fix).
