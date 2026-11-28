---
title: "A forward merge regenerates what its change generates, a base merge reads the base's own CI first, and a verification names each check it could not run"
category: legion
tags:
  - forward-merge
  - release
  - decision-rules
date: 2026-10-21
status: superseded in part by dispatch://INFRA-1569 — no branch holds a contract number since a contract change adds an entry with scripts/daemon-contract-entry.sh, so the registry rule, both entries staying and the manifest sweep went; and by sjawhar/legion#2423 — a pull request no longer rebuilds `dist/envoy-channel.js` or runs `check-dist`, since main's Rebuild Claude Envoy plugin workflow commits the bundle after each merge; the regeneration rule and the base-merge rule stand
module: packages/daemon/internal/api
related_issues:
  - "INFRA-31"
  - "sjawhar/legion#2085"
  - "INFRA-1569"
---

# A forward merge regenerates what its change generates, a base merge reads the base's own CI first, and a verification names each check it could not run

Until dispatch://INFRA-1569 this note also recorded how sjawhar/legion#2085, open for days against
a repository that releases daily, lost the daemon API contract number it held when `main` released
another change at that number, and moved to the next one. No branch holds a contract number since:
a contract change adds an entry with `bash scripts/daemon-contract-entry.sh <slug>`, and a build
numbers its entries in `main`'s merge order. What the branch's forward merges taught beyond the
number stands.

- **What a forward merge moves is regenerated, not edited.** A fixture a Go golden test's `-update`
  writes, a lockfile and a built bundle are each produced again by their generator at the merge
  head; a bundle resolved by eye in a merge is how a wrong artefact ships. End the merge's sweep
  of a changed string with `grep -rn` over the whole tree, `docs/solutions/` included: a file
  written after the merge here still carried the string the merge had replaced, because a list of
  sites was read as the sweep's extent and the grep was not run.

## When a base merge is allowed

Reintegrating the base is for a real conflict only, and on a tree where an architect has forbidden
it for the night the two rules collide: a reviewer's verdict can legitimately require the merge
the standing order forbids without asking. The form that survives both, settled here:

> **A base merge reads the base's own CI first; after that, a reviewer's verdict naming the merge
> is authorisation enough, and you tell the architect rather than ask.**

Merging a red base is how a branch inherits a failure it cannot explain, which is the hazard the
order existed for; running the full suite at the merge head answers the same question from the
other side. An order of the form "ask me first" is incomplete until it says which role's
instruction outranks it.

## Naming what you could not run

A verification section is read by someone who was not there, so a check you could not run is named
with its reason rather than skipped or counted:

- `internal/daemon`'s suite needs Docker, which no role container has — named as CI's.
- `check-bun-version.sh` needs a Python `yaml` module this image lacks — named as CI's.
- A `noTemplateCurlyInString` warning in a test file the branch does not touch is named as
  pre-existing and left alone, neither fixed to make the output clean nor counted as this
  change's.

Each of those is one sentence, and together they are the difference between "everything green" and
a report a reader can check.
