---
title: "Content-free review rounds and implementer jj mechanics"
category: legion
tags:
  - legion-workflow
  - code-review
  - jj
  - handoff-schema
date: 2026-09-23
status: active
module: legion
related_issues:
  - "S390560039-1"
  - "sjawhar/legion-smoke#119"
---

# Content-free review rounds and implementer jj mechanics

Reusable learnings from a Legion implementer that drove pull request
sjawhar/legion-smoke#119 (Dispatch issue S390560039-1) through four implementing
rounds. The change was a single appended README line; three of the four rounds were
triggered by scripted `CHANGES_REQUESTED` reviews that carried no actionable finding.

## 1. A `CHANGES_REQUESTED` review with no finding is not a code defect

A reviewer can submit `CHANGES_REQUESTED` whose only body is a round label, with **zero
inline comments and zero review threads** (`GET .../pulls/<n>/comments` returns `[]`; the
`reviewThreads` GraphQL query returns an empty/`null` node set). When the product already
matches the approved spec exactly, there is nothing in the file to correct — and often
nothing you *may* correct: the approved diff here had to stay exactly one added line
(the acceptance marker) with the preceding lines unchanged, so editing the marker would
break the acceptance criterion and adding any other hunk would break the approved diff.

Correct disposition when the review names nothing and the code is spec-exact:

- Do **not** invent a change to the product file. Confirm spec-compliance against the
  approved plan/spec instead.
- Re-verify on the **real end-user surface** named by the acceptance criterion — for a
  "person viewing README.md on the default branch" criterion that is the GitHub contents
  API (`repos/<owner>/<repo>/contents/README.md?ref=<branch>` decoded from base64), not
  just the local working copy. Include a negative control (the prior last line differs
  from the marker, so a missing/misplaced append is detectable by the same exact-equality
  check).
- Advance the branch head so re-review runs on a fresh commit: re-record the implementer
  handoff (`.legion/implement.json`) with a `reviewResponse` block stating the review id,
  its head, `inlineThreads: 0`, and the reasoned disposition. Commit and push it. The
  handoff commit is a legitimate corrective push; it moves the head without touching the
  product file.
- Run `legion threads resolve --pr <n> --repo <owner>/<repo>` each corrective round and
  paste its output (`no unresolved threads`) into the PR body `Threads` section. Post a
  short PR review-reply comment (with the Legion footer) recording the disposition.
- Keep the PR body's `E2E (implementer)` line pointed at the commit that actually carries
  the change; note in the `CI` line that `README.md` is byte-identical across every pushed
  head, so a green CI run on any of them covers all of them.

## 2. Implementer jj mechanics across interleaved role commits

The issue branch is a jj bookmark. Each round, other roles (e.g. tester, acting as the
review App) add their own handoff commits *above* your last one, but only the implementer
can push. Practical mechanics that worked:

- Build reviewable commits with `jj -R "$WS" split -m "<msg>" <paths>`. When the working
  copy holds only the handoff file, split moves it into its own commit and leaves an empty
  working-copy commit on top — that is expected.
- The bookmark ends up on the empty working-copy commit or on your prior head. Point it at
  the real head with `jj -R "$WS" bookmark set legion/<KEY> -r <rev>`. Moving it *down* to
  a parent (e.g. off the empty `@` onto the handoff commit) needs `--allow-backwards`;
  moving it *forward* across the tester's commit to your new head does not.
- Before pushing, `jj -R "$WS" log -r 'ancestors(@,N)'` and check authorship with
  `-T 'author.email() ++ " | " ++ committer.email()'`. Your commits must show the
  implementer App in both columns; earlier phases' commits legitimately show their own
  role's App. `jj -R "$WS" git push --bookmark legion/<KEY>` then advances the remote,
  carrying the interleaved other-role commits with it.

## 3. `legion handoff write` rejects exactly `schemaVersion` and `phase`

`legion handoff write --phase <p> --data <json>` (Go daemon) validates the `--data` object
against a **blocklist**, not an allowlist: it rejects only the reserved, server-managed
fields `schemaVersion` and `phase` (`handoff data field <name> is not allowed`), and
accepts every other field, including unknown ones. Supply your payload fields
(`summary`, `issue`, `filesChanged`, `acceptanceCriteria`, `proof`, `verification`,
`reviewResponse`, …) and omit `schemaVersion`/`phase`; the daemon writes them itself. An
empty `{}` is accepted, so the schema does not enforce required fields at the CLI — the
`proof`-array requirement described in the skill is a policy you must satisfy yourself, not
a CLI guard in this daemon build.

## 4. Compressed merge-gate ordering in a smoke proof

In this run the normal pre-retro steps (implementer pushes the `.legion/` deletion, the
reviewer approves that cleaned head) were compressed: the reviewer approved a head that
still contained `.legion/`, per an explicit human smoke instruction that overrode waiting
for another round. When a targeted human instruction overrides the standard ordering,
follow it and record what was skipped; retro still writes only `docs/solutions/` and the
Dispatch message, never a `.legion/` file.
