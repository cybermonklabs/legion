---
title: "Two branches took daemon API contract 2 for different shapes: the equality gate passed a plugin that would have failed the handshake, the tester's boot-against-main's-release check caught it, and the branch renumbered to 3"
category: legion
tags:
  - legion
  - daemon-api-contract
  - pi-envoy
  - rebase
  - serial-numbers
  - testing
date: 2026-09-13
status: superseded by dispatch://INFRA-1569 — no branch picks a contract number since then; a contract change adds an entry with scripts/daemon-contract-entry.sh and a build numbers its entries in main's merge order; the note stays as the history that the numbered entries of packages/daemon/internal/api/version.go cite
module: contracts
related_issues:
  - "LEGION-16"
  - "LEGION-20"
  - "LEGION-52"
  - "sjawhar/legion#961"
  - "sjawhar/legion#975"
  - "sjawhar/legion#1018"
  - "INFRA-1569"
symptoms:
  - "the branch daemon boots against main's released plugin and passes the contract gate, although the branch changed request and response shapes the plugin does not know"
  - "two open pull requests both bump LEGION_DAEMON_API_VERSION 1 -> 2 for different reasons"
---

# Two branches took contract 2 for different shapes; the second to land renumbered to 3

This is history. Until dispatch://INFRA-1569 a branch that changed the daemon API picked its own
contract number, and this note recorded the rule by which it did; each numbered entry of
`packages/daemon/internal/api/version.go` that cites it took its number by that rule. No branch
picks a number since: a contract change adds an entry with `bash scripts/daemon-contract-entry.sh
<slug>` and edits no number (`CONTRIBUTING.md`), a build's number is 275 plus its count of
entries, each numbered by its place in `main`'s merge order, and `legion version --contracts`
prints a build's numbers.

`LEGION_DAEMON_API_VERSION` (`packages/contracts/src/legion-daemon-api.ts`) and the plugin
manifest's `legion.daemonApiVersion` (`packages/pi-envoy/package.json`) were compared for equality
when the daemon booted (`verifyLegionPluginContract`). The gate existed because a plugin built
against different HTTP shapes failed the controller/architect boot handshake with nothing in the
daemon's log to say why. Two branches claiming the same number for different shapes was the one
way that gate could be wrong.

## What happened

LEGION-16 (pull request #961, the interactive controller) changed four shapes: `ompSessionFile`
on `controllerLocator` in the state response and on `/controller/ready`, the `/grants` request as
a union with a controller form, and `merge: true` on `/gh-token` only. It bumped
the contract 1 → 2 on both sides in its first pass. While the branch was open, LEGION-20 (#975)
merged its own 1 → 2 for the design gate's `stateGate` and `GatesRegister` shapes, and pi-envoy
1.23.0 through 1.24.x were released from `main` declaring 2.

The branch rebased over #975 cleanly for the contract file — both sides said `2`, so jj saw no
conflict — and every local gate stayed green: `daemon-api-version.test.ts` pinned the constant and
the manifest *equal*, which they were. The tester's round-nine step zero found it: the branch's
daemon, booted against a copy of `main`'s installed 1.24.0 plugin tree, **passed** the contract
gate. That plugin did not know the controller's session file, the grants union, or the merge
flag; its strict schemas would have rejected the first `/legion/v1/state` response
(`controllerLocator: Unrecognized key "ompSessionFile"`) — the silent handshake failure the gate
was built to prevent, with the gate saying everything was fine.

The branch renumbered to 3. The tester's round ten then proved the handshake both ways: the
branch daemon refused 1.23.1 and 1.24.0 by name (`speaks daemon API contract 2; this daemon
requires 3`), refused a contract-1 build, booted against the plugin built from the branch, and
`main`'s schema rejected the state that daemon served.

## The rule the branches followed until INFRA-1569

LEGION-52 had bumped 1 → 2 so a daemon would refuse plugin releases without the credential file;
LEGION-20 took 2 first, and release 1.23.0 — the first to declare 2 — already wrote the credential
file, so LEGION-52 dropped its bump. LEGION-16 was the other half. The rule that covered both was
one question: did the release that first declared the new number already carry the shapes the
branch needed? Where it did (LEGION-52), the branch dropped its bump; where it did not (LEGION-16),
the branch renumbered above it, since a release declaring the number without the branch's shapes
would pass the gate and fail the handshake.

Nothing in either package's suite could catch the collision: the equality test compared two files
on the same branch, and the daemon's gate test built its manifest from the same constant. Only
booting the branch's daemon against the plugin release `main` had shipped saw it, which was the
tester's surface, not the implementer's unit run.

A renumber was a one-line change on each side and a sweep of every sentence that named the old
number, and a second conflict-only rebase followed when #1018 rewrote the same doc comment on
`main` two days later; the branch kept `main`'s history whole and appended its own `3 — LEGION-16`
entry after `main`'s `2`.

## Related

- `schema-bump-branch-rechecks-mains-version-at-every-rebase.md`: the state version's renumber
  rule, which a migration version still follows.
- `../daemon/plugin-daemon-api-contract-version-gate.md`: the gate itself.
- `../daemon/interactive-controller-upgrade-the-headless-pane-probes-alive-and-is-killed-once-after-the-restart.md`:
  the deployment order that installed the contract-3 release before the daemon restarted.
