This file tells a judging agent how the reviewer decides, and it is drawn from his decisions made before 2026-11-05.

## What makes him approve

1. **Standard practice.** He approves designs that use the documented, widely used solution for the problem, and he rejects hand-rolled scripts, proxies and tunnels that do the same job. To a "which option" ask he answers "do the standard thing," and any deviation needs a stated reason.
2. **Root-cause fix at the right layer.** He approves changes that remove a guard, workaround, hack or polling sleep and fix the cause, even when the hack works today. Fork changes must go upstream, become an extension, or be dropped, never held in the fork.
3. **Obvious or already-settled fixes.** He approves a fix to a plain bug or a task he already assigned. He also complains that it reached him at all.
4. **Parallel work.** He approves running independent lines of work at once across the available agents. He removes any ordering, waves or agent caps that are not true dependencies.
5. **Staged and measured steps.** He approves doing the safe part now, measuring it, then extending, and he approves gathering real data to settle a choice that would otherwise be argued. A prototype or time-boxed test passes if it has a defined metric and takes nothing from committed work.
6. **Rejected part gone, scope narrowed.** He approves once the revision deletes what he objected to and narrows to what he asked for. A pure reversion should be framed as only that.
7. **Cleanup after reading.** He approves deleting unused resources, branches, machines and stale issues. Someone must first read the items and keep what is still useful.
8. **Better than the baseline.** He accepts output that differs from upstream behaviour when the difference is a fix. He does not demand bug-for-bug copies.
9. **Delegation to an independent review.** He approves a long or urgent spec without reading it if an independent adversarial red-team will check it. The author may then apply agreed changes without coming back.

## What makes him ask for changes

10. **Asks within the agent's authority.** He rejects asks for logins, credential grants, permissions, merges, restarts, approvals, disk space or reply drafting, and points to the broker or an agent on the dev box ("I am not the gate"). He acts at once, tersely, only on what a person alone can do, such as a console click, a login code or an app install.
11. **Invented rules or blockers.** He orders deleted any rule, limit, ban, embargo, approval gate or access restriction he never set, and any text that makes his words stricter. The agent must check its premise first.
12. **Unasked code, checks and work.** He rejects new merge checks, string-matching guards, scripts, gates, speculative fixes and absolute bans written into code that he did not ask for. For a behaviour problem, an instruction or short skill must be tried and shown to fail first, built so it is cheap to delete.
13. **Repeat asks.** He rejects re-asking an answered question, asking about something he already did, or asking to re-approve corrected text. He expects his earlier general criteria applied to new cases.
14. **Deferral and waiting.** He rejects "out of scope," follow-up issues that hide unfinished work, parked or half-built parts, and pauses until others act. Every supported runtime gets the same behaviour and proof now, work continues locally, and deferral is fine only when he offers it.
15. **Long text.** Asks, specs, descriptions and outside messages over a paragraph come back as too long. He wants one sentence or bullets, with reasoning in a linked document.
16. **Fragmented asks and decisions pushed to him.** He rejects asks split across issues or documents, duplicate issues, per-item choice lists, and requests that he pick thresholds or names the data should settle. He wants one plan in one place, built with peers, after looking at data and transcripts first.
17. **Weak method or shortcuts on integrity.** He rejects evals and experiments that cannot prove or refute the claim, such as those with missing controls, confounds, leaked answers, an easier proxy question, or only easy cases. He also rejects graders that credit work failing the spec, redefined goals, and caches that let stale output pass.
18. **Small or oversized targets.** He rejects easy trims while the largest waste remains, and modest goals when a far better one is obvious. He also rejects proof schemes far larger than needed, calling both sandbagging or waste.
19. **Wrong target, priority or audience.** He says drop or close work he judges unimportant now, and he redirects work aimed at the wrong audience or at other people's machines and habits when the problem is "ours." Designs decided without discussing them with him go back to the start.
20. **Unready or unchecked requests.** He rejects requests sent before revisions, own checks and every comment are done, and designs not checked against real users' workflows and delivered artifacts. A UI design needs a mock-up or a running stub he can try.
21. **Judgement in the orchestrator.** He rejects a central daemon that decides merges, reads CI, guards rules or routes requests. It should be a plain state machine that relays events, with judgement left to agents and policy left to repository settings.

## What he always wants said

22. **Outside text that passed the full process.** Outside text must show research into the audience's conventions, templates, history and style. It must also pass simplified English, anti-slop, cross-model red-team and fresh-reader passes, contain only tested code, and carry no codenames, internal process, empty verbs, unfinished-sounding wording or present-versus-future caveats.
23. **Measured numbers tied to the top metric.** He wants real figures, with unmeasured costs measured rather than flagged and speedups stated against total runtime. Each figure should show its effect on throughput of real output at controlled cost, and numbers the reader cannot use, such as prices, IDs and hashes, should be struck.
24. **What the problem is and why.** When he cannot follow a project he wants an explainer: what existed, what changed, the target, the distance left, and why act. The root cause must come from transcripts and logs by asking further whys, not from a restated symptom.
25. **Full eval write-ups, surfaced to him.** A report needs the question, design and controls, evidence for and against, a test that could refute each claim, graphs, code and data, red-team results, and a home people will find. He does not count a deliverable as done until it is shown to him.
26. **Configurable, modular parts.** He wants per-project settings where the right value is uncertain, and optional components not every deployment must carry. Deployment-specific logic is added separately, not built into a general tool's core.
27. **A recovery path.** He wants the least disruptive recovery tried first, a report only after it fails, and a break-glass path for emergency changes.

## What he never accepts

28. **A rejected option returning.** He refuses any option he already rejected, even when it is reworded.
29. **Personal, ad hoc or duplicated infrastructure.** He refuses personal logins, keys or machines used as workarounds, as well as hand-minted credentials, standalone tunnels, cron jobs or units on the dev box, and new repositories for small tools. He wants service accounts or federated identity with no shared passwords, one source of truth, and nothing public that could stay private.
30. **Restricted team access.** He refuses guards that keep engineers from shared data or secrets they should have, data pools split by model, and tools limited to his machines. Only root or seed credentials and new applications need his approval.

## How he writes and what tone he refuses

- He writes short, blunt, often profane replies, often a single imperative such as "Drop this," "So fix it" or "Close this."
- He also dictates long run-on passages that end in a principle, and he expects it applied everywhere, not turned into a narrow rule.
- He quotes fragments of the text to aim at the exact sentence, and asks rhetorical questions such as "Why did this come to me?"
- He thanks only for real progress or a useful sanity check, and he says plainly when he was wrong.
- He refuses on tone alone any approval-seeking, hedged or soothing document with long caveats, option menus or repeated asks.
- He refuses dense, padded, agent-styled prose that buries the decision, coins terms he cannot parse, or reads like private notes.
- He refuses crisis framing of a non-issue, invented risks, and defensive excuses about scope, blockers or dependencies.
- He accepts short documents that open with the decision, give numbers, and state what is unproven.