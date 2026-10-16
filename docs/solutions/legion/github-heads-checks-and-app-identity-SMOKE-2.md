---
title: "Reading GitHub heads, check runs and App identity in a Legion run"
category: legion
tags:
  - github
  - check-runs
  - skip-checks
  - bot-identity
  - merge-gate
date: 2026-10-16
status: active
module: legion-worker
related_issues:
  - "SMOKE-2"
  - "sjawhar/legion-smoke#589"
---

# Reading GitHub heads, check runs and App identity in a Legion run

Three GitHub facts cost rounds when a phase assumes them instead of reading them at its own head.

- The head that merges must be a code head. A handoff-only push ends its message with
  `skip-checks: true`, so GitHub starts no workflow for it and that head carries zero check runs;
  a required check can never be satisfied there. Read mergeability at the exact SHA you nominate,
  and nominate the no-trailer head.
- That refusal is also the honest negative control for "the required check is enforced", and it
  costs nothing: a head with no check runs reports `BLOCKED` with an empty rollup. Confirm from
  the ruleset that no other rule explains the block before you read it that way.
- Evaluate check runs as a set. A head that received both a `push` and a `pull_request` event
  carries one row per event for every check name. Select over all rows and require every
  conclusion; a phase that matches the first row can report green while a sibling row is red.
- Resolve bot and App identity at the head you are proving, and pin the numeric account id rather
  than a login string. An identity literal inherited from an earlier phase, an earlier run, or
  another deployment is unverified input.
- Write identity acceptance criteria as properties ("written by Legion's implementer App"), not as
  string literals. A deployment that re-installs its App changes the login and fails a literal
  while the property still holds.
- Read the required-check set from the branch ruleset when branch protection refuses your App
  (403). Record the refusal as a measured fact; do not work around it or assume the set.

## Evidence (SMOKE-2, sjawhar/legion-smoke#589)

- `skip-checks` heads: at `9287004957ea45c48d0fe9b6e0b4f85a4e526813` (a handoff-only commit whose
  message ends `skip-checks: true` after two empty lines),
  `gh api repos/sjawhar/legion-smoke/commits/<sha>/check-runs --jq .total_count` → `0`, and
  `gh pr view 589 --json mergeable,mergeStateStatus` → `MERGEABLE` / `BLOCKED` with an empty
  `statusCheckRollup`. The ruleset on `main` carries exactly one rule,
  `required_status_checks` → `gate`, and no required-review rule, so the block is provably the
  missing `gate`.
- Duplicate rows: at the code head `71461a80bf63c57b093ee4389f3f0d34a7eaa932`, `gate` and `check`
  each appear twice, matching `fail-on-demand` runs `38921570750` (push) and `38921816497`
  (pull_request) and `ci` runs `38921570791` (push) and `38921816449` (pull_request), all
  `success`.
- Identity: the plan pinned Legion's implement App as `trajectory-s-legion-implementer[bot]`,
  measured on pull request 582 of an earlier deployment against the same repository. At this run's
  head the author is `legion-implementer[bot]`, account id `343353655`, matching the commit author
  email `343353655+legion-implementer[bot]@users.noreply.github.com` and `pulls/589 .user.id`. The
  acceptance criterion read "written by Legion's implementer App", a property, so it held; the
  implementer recorded the stale literal as a deviation in its handoff and sent it to the
  architect, which confirmed it live and had the planner correct the plan. Caught in the
  implementing phase, it cost no round; compared blindly at the merge commit it would have failed
  a passing run.
- Required set under a 403: branch protection refused this App, so the set came from
  `gh api repos/sjawhar/legion-smoke/rules/branches/main --jq '.[]|select(.type=="required_status_checks")|.parameters.required_status_checks[].context'` → `gate`, and the refusal was recorded in the plan rather than worked around.
