---
title: "Required GitHub App Checks on main"
category: github
tags: ["github", "branch-protection", "checks", "github-apps"]
date: "2026-07-19"
status: "active"
---

# Required GitHub App Checks on `main`

`main` must require these checks before a pull request can merge:

| Check | Publisher App | Source pin |
| --- | --- | --- |
| `CI` | GitHub Actions | App ID `15368` |
| `tester` | Legion reviewer App | `REVIEWER_APP_ID` |
| `architect` | Legion reviewer App | `REVIEWER_APP_ID` |

The reviewer App must also submit an approving pull-request review. Branch
protection requires one approval. GitHub's branch-protection REST API has no
field that restricts the approving reviewer to one App, so source pinning is
available for check runs only. The deployment verification below confirms that
the reviewer App can submit the `tester` check; validate the review workflow
separately before relying on the approval requirement.

## Sequencing

**Do not apply this policy until CI can publish `CI` and the reviewer App can
publish both bare `tester` and `architect` checks.** Applying it earlier blocks
every merge, including the pull request that adds the check-publishing pipeline.

Before applying, verify a scratch pull request head contains all three names and
the expected App IDs:

```bash
gh api -H 'Accept: application/vnd.github+json' \
  "repos/cybermonklabs/legion/commits/<scratch-pr-head-sha>/check-runs" \
  --jq '.check_runs[] | select(.name == "CI" or .name == "tester" or .name == "architect") | {name, app_id: .app.id, app_slug: .app.slug}'
```

The initial U5 inspection found GitHub Actions App ID `15368` on existing job
checks, but no bare `CI` check on the sampled open PR. That prerequisite must
be satisfied before the policy is applied.

## Verify `tester` with the reviewer App

Use a disposable scratch PR, not a production pull request. Mint an installation
token for the reviewer App, keep it in `REVIEWER_APP_TOKEN`, and set
`REVIEWER_APP_ID` to that App's numeric ID.

```bash
scratch_pr=<scratch-pr-number>
head_sha="$(gh pr view "$scratch_pr" --repo cybermonklabs/legion --json headRefOid --jq .headRefOid)"

created="$(GH_TOKEN="$REVIEWER_APP_TOKEN" gh api -X POST \
  -H 'Accept: application/vnd.github+json' \
  "repos/cybermonklabs/legion/check-runs" \
  -f name=tester \
  -f head_sha="$head_sha" \
  -f status=completed \
  -f conclusion=success)"

test "$(jq -r .name <<<"$created")" = tester
test "$(jq -r .head_sha <<<"$created")" = "$head_sha"
test "$(jq -r .app.id <<<"$created")" = "$REVIEWER_APP_ID"

GH_TOKEN="$REVIEWER_APP_TOKEN" gh api \
  -H 'Accept: application/vnd.github+json' \
  "repos/cybermonklabs/legion/commits/${head_sha}/check-runs" \
  --jq '.check_runs[] | select(.name == "tester") | {id, name, head_sha, status, conclusion, app_id: .app.id, app_slug: .app.slug}'
```

Record the created run's `id`, `head_sha`, `app_id`, and `app_slug` with the
deployment evidence. Do not use an implementer-App token: a run made by that
App does not prove the reviewer App can publish the required verdict.

## Apply the policy

One `gh api` call applies it. It needs a credential with repository
Administration write access, and it never runs as part of CI. Set
`REVIEWER_APP_ID` as in the verification above; the command stops before it
sends anything when that variable is unset.

```bash
gh api --method PUT -H 'Accept: application/vnd.github+json' \
  repos/cybermonklabs/legion/branches/main/protection --input - <<JSON
{
  "required_status_checks": {
    "strict": true,
    "checks": [
      {"context": "CI", "app_id": 15368},
      {"context": "tester", "app_id": ${REVIEWER_APP_ID:?}},
      {"context": "architect", "app_id": ${REVIEWER_APP_ID:?}}
    ]
  },
  "enforce_admins": false,
  "required_pull_request_reviews": {
    "dismissal_restrictions": {"users": [], "teams": [], "apps": []},
    "dismiss_stale_reviews": false,
    "require_code_owner_reviews": false,
    "required_approving_review_count": 1,
    "require_last_push_approval": false,
    "bypass_pull_request_allowances": {"users": [], "teams": [], "apps": []}
  },
  "restrictions": null,
  "required_linear_history": false,
  "allow_force_pushes": false,
  "allow_deletions": false,
  "block_creations": false,
  "required_conversation_resolution": false,
  "lock_branch": false,
  "allow_fork_syncing": false
}
JSON
```

`15368` is the GitHub Actions App ID observed on the sampled PR. Change it only
if the CI publisher changes.

The PUT replaces the branch's whole protection. If `main` is already protected,
read what it holds first (`gh api repos/cybermonklabs/legion/branches/main/protection`)
and copy into the JSON every other required check, the current `strict` value and
any approval count above one. Otherwise the PUT drops them. Running the same
JSON again gives the same policy.
