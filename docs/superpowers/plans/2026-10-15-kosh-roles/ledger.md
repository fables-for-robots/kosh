# Ledger of the build

The controller's record of the subagent-driven build of the roles feature: for each task its commits, the review, the rulings, and what was parked. The rulings are the places to look when something here turns out wrong. (The working ledger, with the paths of the review packages, lives in the git-ignored SDD workspace; this is its copy, with the paths dropped.)


Branch `roles` on main `3f04cbd` (v0.4.0). Spec 59c5ca7, plan committed next.

## Pre-flight scan

| Pair / task | Produces vs consumes | Found |
|---|---|---|
| T1 → T2/T5/T6 | `Grant{Name,Fingerprint,Role}`, `Role`, `User()`, `Group()` | agree |
| T2 → T3/T4 | `identity`, `clients`, `cacheKey`, session clients | agree |
| T3 → T5 | `Reader.Can`, `kubetest.Allow` | agree |
| T4 → T6 | `Bindings`, OnChange order (make, end, prune) | agree; T4 wires cli, T6 confirms |
| T2 alone | repo does not build until T5 (ReadOnly removal cascades) | ruled: accepted; commits say so; CI on the PR |
| T2 alone | envtest RBAC mode must be on | T2 verifies/sets |

Ruling: the spec was shown to the user; their one comment (start in the Role namespace) is in; proceeding without a further review — cost if wrong: rework visible in the PR.

## Task 1
BASE 1955634. Implementer dispatched (opus).
Implementer 7c2a470 DONE_WITH_CONCERNS (space/invisible chars rejected beyond IsValidPathSegmentName; Kind constants; msgNoAdmin gone; case-sensitive roles). Reviewer dispatched (opus).

## Task 2
BASE 7c2a470. Implementer dispatched (opus) in parallel with the Task 1 review.
Review : spec ✅, quality needs work — Important: unprintable/bidi characters in the name part can disguise the role; minors: exact reasons in tests, long apimachinery messages. Ruling: fix all three; the cache user-name collision noted for Task 2 (ClusterRole team-a:edit vs Role team-a/edit) — told the Task 2 implementer to mirror Group() prefixes; spec §3.3 to be amended. Fix round 1 dispatched.
Fix round 1: 0eaaaa0 (whole comment must print; exact reasons; own words for bad role/namespace). Scoped re-review dispatched (sonnet).
Re-review fix round 1: all three addressed; no new defect.
Task 1: complete (7c2a470, 0eaaaa0).
Implementer aca2c7f → amended 0a7b038 (trailer) DONE: pollers keyed as caches (role, kind, ns); Role usage watch never served; table.go refusal bug fixed; envtest view role filled from aggregate-to-view rules; concerns: per-identity rate limiters, clients of removed keys kept, refused describe untested. Reviewer dispatched (opus).

## Task 3
BASE 0a7b038. Implementer dispatched (opus) in parallel with the Task 2 review.
Review : spec ✅, quality approved; 7 minors. Ruling: fix 1 (warn on a refused action), 2 (no config → not the SA clients: test-only, fail closed), 3 (document per-identity limiter, keep), 4 (impersonation asserted on SecretData/Owner/Logs/generic table), 5 (refused describe test, apiStatusOf unit test), 7 (comments); park 6 (begin returning clients). Fix round after Task 3 lands (same package, same checkout).
Task 3 implementer 10931a2 DONE (can cache on session; TestCanAsksAsTheKey added; fake Can not in Calls). Reviewer dispatched (opus). Task 2 fix round 1 dispatched (same time; files disjoint from the review).
Review : spec ✅, quality needs work — Important: fake Allow(ns "") must match every namespace (RBAC semantics); minors: cancelled-context case, DeadlineExceeded hidden (note for Task 5), canError comment. Ruling: fix the Important and the two test minors; log deadline errors too. Fix round after the Task 2 round lands.
Task 2 fix round 1: 0d9341a (also: Delete of a Secret keeps API errors for the log). Scoped re-review dispatched (sonnet). Task 3 fix round 1 dispatched.
Re-review Task 2 fix round 1: all six addressed; cosmetic: a broken comment wrap in cluster.go newCluster (parked for the final review or a later touch).
Task 2: complete (0a7b038, 0d9341a).
Task 3 fix round 1: 4f8423b. Scoped re-review dispatched (sonnet).

## Task 4
BASE 4f8423b. Implementer dispatched (opus) in parallel with the Task 3 re-review.
Re-review Task 3 fix round 1: all three addressed; no new defect.
Task 3: complete (10931a2, 4f8423b).
Task 4 implementer 6faa0f9 → amended fcb874b (trailer) DONE: Run(ctx) retry-only, MakeNow/PruneNow exported; 10 s reconcile timeout on a change; label added to an unlabelled binding at kosh's name; concerns: §7 minimum lacks get on roles (ruling: add get to the minimum), warning names role not keys (ruling: fix), ERROR every 30 s for a Role in a missing namespace (ruling: once per failure run). Reviewer dispatched (opus).

## Task 5
BASE fcb874b. Implementer dispatched (opus) in parallel with the Task 4 review (tui only).
Review : spec ❌ (Important: warning lacks keys; Forbidden get swallowed → add get to the minimum), quality approved; minors. Ruling: fix both Importants and minors 3–7 (adopt only when no managed-by label; deadlines after the lock; failure logged once per run; RB for real in envtest; nits); annotation kept as built (spec amended 42b9b48). Fix round 1 dispatched.
Task 5 implementer d0c16db DONE (rightsGen separate from gen; concurrent Can asks; harness hold/release for the no-keys-yet test; TestShellReadOnlyKeyRefused removed; concern: an open help does not refresh when answers land). Reviewer dispatched (opus).
Task 4 fix round 1: a510c6f → amended f6cf6c1 (trailer): keys in the warning (List.Roles() → []RoleKeys, Set.KeysOf), get on roles in the minimum, foreign managed-by left alone, deadlines per call, failure logged once per run, RB for real in envtest. Scoped re-review dispatched (sonnet).
Re-review Task 4 fix round 1: all seven addressed; no new defect.
Task 4: complete (fcb874b, f6cf6c1).
Review : spec ✅, quality approved; 4 minors. Ruling: fix 1 (dedupe needs), 2 (help rebuilds on rightsMsg), 3 (ctrl+r re-asks); park 4. Fix round 1 dispatched.

## Task 6
BASE f6cf6c1. Implementer dispatched (opus) in parallel with the Task 5 fix round (disjoint files).
Task 5 fix round 1: 60e447e (rights asked once per (verb,group,resource,sub); help reads its table's keys at draw; ctrl+r re-asks). Scoped re-review dispatched (sonnet).
Re-review Task 5 fix round 1: all three addressed; no new defect.
Task 5: complete (d0c16db, 60e447e).
Task 6 implementer 99e2217, 43bcac9, cc5aa1b DONE: suite green with envtest; e2e on the box 54 pass (second run; first failed a test typing before :pods opened — fixed); box clean; concerns: any grant change ends a session (ruled: right under impersonation; spec §3.6 to say so), 90 s RoleBinding deadline tight (ruled: raise), test-mdns.sh changed but not run (ruled: run on the box in the final round). Reviewer (opus) and the final whole-branch review (opus) dispatched together.
Review : spec ❌ (README metrics example with a role that cannot list nodes), quality needs work: Important — test-mdns.sh pipefail bug leaves the view binding; a second kosh in one cluster prunes the first's bindings (shared label and names) — ruling: an instance name (`--instance`, chart `<namespace>-<release>`, default `kosh`) in the group and binding names (`kosh:<instance>:clusterrole:<role>`), spec §2/§4 amended; the 90 s RoleBinding deadline → 150 s; minors: help checked for "no action keys", comment on "made before", README reflow, naming leftovers. One fix dispatch after the final review.

## Final review (final-review.md)
Verdict: ready after the Important findings. No Critical (no session request as the SA). I1 instance isolation (= Task 6 review), I2 escalation via the key list ConfigMap. Minors M1–M11. Ruling: spec amended 28351fc (§2 instance, §4, §4.1); one fix dispatch (Task 6 implementer) for I1, I2, the Task 6 review items, and M1, M2, M4, M5, M6, M7; park M3, M8, M9, M10; e2e + mdns re-run on the box required (names change).
Final fix round: ee43f05, e59d060, 17da339 (instance in every name, --instance/--namespace, §4.1 warning, Task 6 items, M1/M2/M4/M5/M6/M7; e2e 54 pass and mdns pass on the box; box clean). Scoped re-review dispatched (opus).
Re-review final fix round: all addressed; ready for the PR; two new minors — CheckInstance accepted a leading/trailing - or . that the label refuses, and the chart helper stripped one trailing - only (fixed by me: the regex, regexReplaceAll with the right argument order — the pipeline form rendered an empty instance); the editors warning not re-run after a Run retry (fixed by me: Run calls WarnEditors after a Make that works; TestRunWarnsEditorsAfterARetry fails on the old code). editorsUnchecked once per Cluster life: parked.

## Closing
Ledger copied to docs/superpowers/plans/2026-10-15-kosh-roles/ledger.md; Departures table has the final round.
