---
title: "Proving a fact the event you are proving erases"
category: legion
tags:
  - proof
  - negative-control
  - github-api
  - auto-merge
date: 2026-10-16
status: active
module: legion-worker
related_issues:
  - "SMOKE-3"
  - "sjawhar/legion-smoke#599"
---

# Proving a fact the event you are proving erases

Some claims are about a state that the event they describe destroys. "Auto-merge was never
armed on this pull request" is one: GitHub serves `pulls/<n>.auto_merge` as `null` on every
merged pull request, armed or not, so after the merge the field answers the question the same
way whatever the truth was.

- Prove such a claim from the **durable record** — the issue timeline's event list
  (`gh api "repos/{owner}/{repo}/issues/<n>/timeline?per_page=100" --paginate --jq
  '[.[].event]|unique'`), which keeps `auto_merge_enabled` (or its absence) after the merge.
  Put that read in the **first** proof that makes the claim, not in a later phase's.
- Treat the live field as corroboration, and stamp every read of it with its UTC time **and the
  object's state at that moment**; a read whose window has closed proves nothing, and a reader
  cannot tell which kind they are looking at without the stamp.
- Do not attach a pull-request-level field to a commit SHA in the evidence line. `auto_merge` is
  a property of the pull request; writing "at head `<sha>` … `auto_merge` is null" invites a
  reader to believe the evidence is head-pinned and therefore recoverable later. It is not.
- Say what the samples support. Four timed reads of a field prove four instants; only the
  timeline's absence of the event supports "across its whole open life".

The same shape applies to any erase-on-completion fact: draft state, a required-reviewer
waiver, a branch-protection bypass, a queue entry. Ask first what survives the event.

## Negative controls for a claim of absence

A negative control is **the same read with one thing deliberately wrong**, and its recorded
refusal is what gives the read its meaning.

- A read of a *neighbouring field* is not a control for your field. `allow_auto_merge` → `true`
  on another repository shows that repository setting is read rather than assumed; it says
  nothing about whether `.auto_merge` would report an arming if one existed.
- For a claim of absence, add a **filter-sensitivity** control: run the identical pipeline
  selecting an event that *is* present (`select(.event=="committed")` → 4 rows). That turns
  "the filter found nothing" into "nothing is there", not "the jq path is wrong".
- When the real positive control is unreachable or forbidden, name the sensitivity that stays
  unproven in the pull request body's `Not proven / risk`. Never let a neighbouring-field
  control imply it.
- When you re-verify another phase, rebuild its reference artefact **from the specification**,
  not from the artefact that phase produced, and re-run at a head it never proved. A `cmp`
  against the author's own scratch file compares the author with themselves.

## Evidence

SMOKE-3 (`sjawhar/legion-smoke#599`) had to record on `main` that Legion's daemon merged the
run's READY pull request rather than GitHub auto-merge. The implementer's own handoff named the
hazard — "auto_merge is null on a merged pull request whether or not it was ever armed" — and
still cited only two field reads (18:55:09Z, 18:56:07Z) in its proof entry. The durable timeline
read appeared one phase later, in the tester's handoff ("exactly one event kind, 'committed', and
no auto_merge_enabled"), with the `select(.event=="committed")` → 4 rows sensitivity control; the
reviewer added a fourth field read at 19:09:21Z. The claim survived only because the later phases
supplied what the first proof lacked. The implementer's single control for that entry was
`allow_auto_merge` on another repository — the neighbouring-field case above. A positive control
was declined by policy (no phase arms auto-merge anywhere to manufacture one) and the residual
gap was carried in the body's `Not proven / risk`, which is the disclosure this rule asks for.

The tester's independence measures are the reference-rebuilding case: it rebuilt the 299-byte
reference from the plan's own `printf` rather than reading the implementer's `/tmp/smoke3-ref.txt`,
and repeated every read at a head the implementer never proved. Both `cmp`s were silent.
