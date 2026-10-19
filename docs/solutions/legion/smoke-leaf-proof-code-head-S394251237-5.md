---
title: "Prove a legion-smoke leaf against the code head, not the handoff-only PR head"
category: legion
tags:
  - legion-smoke
  - proof-surface
  - skip-checks
  - ci-verification
date: 2026-10-19
status: active
module: legion-smoke
related_issues:
  - "S394251237-5"
  - "sjawhar/legion-smoke#700"
---

# Prove a legion-smoke leaf against the code head, not the handoff-only PR head

A legion-smoke issue branch carries two distinct heads, and CI runs on only one of them. Verify
the change against the right one.

- A leaf issue (`-5`) adds exactly `smoke/<KEY>.md` whose sole content is the 13 bytes `<KEY>\n`:
  UTF-8, one trailing LF, no CRLF, no BOM, no blank lines. Create it with
  `printf '<KEY>\n' > smoke/<KEY>.md` and assert `printf '<KEY>\n' | cmp - smoke/<KEY>.md` plus
  `wc -c` = 13. (`-1` issues instead carry spec text in the same file.)
- Prove the file on the production-like surface a consumer reads it through — the GitHub contents
  API at the pushed head — not the local working copy:
  `gh api repos/<owner>/<repo>/contents/smoke/<KEY>.md?ref=<sha> --jq .content | base64 -d | cmp - <(printf '<KEY>\n')`.
  Negative control: the same fetch at the parent head returns `404 Not Found`.
- The handoff-only push (touching only `.legion/`) carries a `skip-checks: true` trailer, so GitHub
  starts no workflow for that head and `gh pr checks` reports none. Read CI from the **code head**
  (the commit that added the file), via the check-runs API:
  `gh api repos/<owner>/<repo>/commits/<code-sha>/check-runs --jq '.check_runs[] | {name,conclusion}'`.
  The required checks are `check` (workflow `ci`) and `gate` (workflow `fail-on-demand`); the
  daemon carries the code head's verdict to the handoff head.
- `gate` is red only when a root `.fail-me` exists. It is the deliberate negative-control lever and
  MUST NOT be committed on a passing branch.

## Evidence

Issue S394251237-5, PR sjawhar/legion-smoke#700. The code head `f56d6056` showed `check` and
`gate` both `success`; the handoff-only head `6d1403bd` (skip-checks) reported no checks. The
contents API served byte-exact `S394251237-5\n` at the code head and `404` at the parent
`08d71bdf`.
