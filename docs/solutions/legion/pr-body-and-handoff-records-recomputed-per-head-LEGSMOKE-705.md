---
title: "The PR body and the handoff JSON are ledgers: recompute them from the API at every head, and keep each command re-runnable"
category: legion
tags:
  - pr-body
  - handoff
  - size-line
  - record-quality
  - command-strings
date: 2026-10-13
status: active
module: legion-worker
related_issues:
  - "LEGSMOKE-705"
  - "sjawhar/legion-smoke#561"
---

# The PR body and the handoff JSON are ledgers: recompute them from the API at every head, and keep each command re-runnable

- Fetch the live body (`gh api repos/{o}/{r}/pulls/{n} --jq .body`) into a fresh temp file immediately before every edit; never edit a copy kept from an earlier round. Locate lines by their `**Label:**` prefix, not by line number, and run the required-line census (`Dispatch:` first line, both `E2E` lines, `Not proven / risk:`, `Chain:`, `Fast-follow:`, no `Thermo:` on a docs-only PR) after each `PATCH`.
- `Size:` goes stale twice per round: once at the code push and again at your own handoff push, which adds or grows a `.legion/` file. Re-derive it from `gh pr view --json changedFiles,additions,deletions` and `pulls/<n>/files` after the handoff push too, and name the head it describes.
- Every clause in the body names the SHA it is true at; a `CI:` line names each head and its run ids, including a red head and why it was red. Never write "green on every head".
- A handoff `proof[].command` is re-runnable verbatim from a fresh directory: full SHAs (no `…`), helpers (`dec()`, `L1`) defined inside the same string, repository and PR number spelled out. The tester re-runs these strings; an elided one decodes to nothing.
- Keep a key's JSON type stable across phases (`round` an integer, `kEvidence` an array of objects) and say what `round` counts: each phase's own history, not a cross-phase index, unless the plan defines one.
- A rejected `envoy_publish` (`no holder for role`) does not block completion: the committed handoff and the daemon's completion notice carry the record; resend when the role reappears.

## Evidence

- PR #561, round 1: a line-number edit replaced the `Not proven / risk` bullet and duplicated `Size:` and `CI:`; the body was rewritten whole from the live copy and the census passed. Later rounds fetched the live body first and edited by label.
- `Size:` was re-derived at 2de1cd5d (+74), 4f91a8d4 (+159), 825b17ed (+159), 467ea956 (+191), 60c2c57c (+322), ff04c7fd (+367): the handoff pushes moved it as often as the code pushes.
- Reviewer round 1 recorded three Minor record findings: `test.json:23,31,63` elided SHAs and a helper defined in one string and used in another; `round` a prose string in `implement.json` vs an integer in `test.json`; `kEvidence` `[]` vs prose. Round 2 closed all three by rewriting the handoffs whole.
- The architect topic had no holder when round 1 completed (01:10Z); the report was delivered at the next completion, and nothing was lost because `implement.json` and the body held it.
- Predecessor (removed from `main` by #560, in history at 152231add64a): `docs/solutions/legion/pr-body-claims-recomputed-per-head-LEGSMOKE-692.md`; this document restates its rule for a fresh tree and adds the handoff-push and command-string rules.
