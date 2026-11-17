---
title: "An unknown GitHub event published an empty envelope to the repository's `comment` topic: prove it with a publish count, not a status code, and publish only the events the listener normalizes"
category: envoy
tags:
  - webhook
  - github
  - merge_group
  - normalize
  - commit-status
  - tdd
  - listener-rollout
date: 2026-09-14
status: active
module: envoy
related_issues:
  - "LEGION-99"
  - "sjawhar/legion#1089"
  - "LEGION-114"
symptoms:
  - "a session subscribed to notifications.github.<owner>.<repo>.> receives a delivery whose summary is `<event> <action>` (e.g. `merge_group checks_requested`, `status `) with an empty payload"
  - "a Legion daemon logs `poison JetStream message` with `Envoy envelope payload is not JSON` on notifications.github.<owner>.<repo>.comment"
  - "a spec says the webhook must 'answer 200 and publish nothing' for an event and the handler already answers 200"
---

# An unknown GitHub event published an empty envelope to the repository's `comment` topic

Envoy's GitHub webhook (`packages/envoy/internal/webhook/github.go`) does not refuse an event type
it has never heard of. Until cybermonklabs/legion#4315 it normalized one through defaults that each
looked harmless and together published one contentless envelope. An earlier change met this with
GitHub's `merge_group` event and skipped that event by name; #4315 fixed the default itself,
after a local CI's commit statuses made every Legion daemon of five repositories log hundreds of
poison lines a day.

## Where the empty envelope came from

In `packages/envoy/internal/contracts/normalize.go`, for an event with no case of its own:

- `githubKind` fell through to `"comment"`;
- `githubNumber` returns `""`, so `githubTopic` falls through to
  `GithubSubject(owner, repo, "comment")` — the **repository-wide** `notifications.github.<owner>.<repo>.comment`;
- `githubSummary` falls through to `fmt.Sprintf("%s %s", event, action)` — non-empty, so
  `Validate()`'s `payload_summary` check passes;
- `githubPayload` falls through to `""`, and `Payload` is not a required field of the generated
  envelope schema, so `Validate()` passes;
- `GithubEnvelopes` returns that one envelope, and the handler publishes it and answers 200.

Every consumer of the repository-wide topic then saw a delivery with summary `merge_group
checks_requested` or `status ` and nothing else. `Payload` is `omitempty`, so the stored message
carried no `payload` key at all. The Go Legion daemon's durable consumer (`notifications.github.
<owner>.<repo>.>`) refuses an envelope whose payload is not JSON and terminates it as poison, one
ERROR line per delivery (`valid` in `packages/daemon/internal/intake/decode.go`).

## The fix: publish only what the listener normalizes

The topic kind no longer falls through to `"comment"`: an event `githubEventKinds` does not name has
no topic, and `GithubEnvelopes` publishes no envelope whose topic or payload is empty. An unknown
event is answered 200 and published nowhere, after the signature check, because the handler
reaches `GithubEnvelopes` only after it. `githubSkip` went with the default it worked around.
`TestGithubEnvelopesPublishNoEventTheListenerDoesNotNormalize` holds a sample of GitHub's events
to that, and `TestEveryPublishedGithubEnvelopeCarriesAJSONObjectPayload` publishes every event the
table names from a body of its own, so an event added to the table without a payload case fails
there and not in a daemon's log (`sub_issues` was one, and now has its payload).

## Prove "publishes nothing" with a publish count, never with the status code

LEGION-99's spec first read "changed only if the current handler errors on the event". The handler
does not error — it answers 200 — and a check written as "does it 200?" would have passed and
shipped the junk envelope. The planner found the defect by running the handler with a documented
`merge_group` payload and reading the publisher: `status=200, published=1,
topic=notifications.github.sjawhar.legion.comment, payload=''`. The assertion the requirement
needs is `len(pub.published) == 0` **and** `len(recorder.calls)+len(recorder.suiteCalls)+
len(recorder.headCalls) == 0`, with the status as a third check, not the only one.

The test that carries it (`TestGitHubHandlerMergeGroup` in `github_test.go`) was written first and
run red before the handler changed — `published = 1, want 0; first topic
"notifications.github.sjawhar.legion.comment"` on both signed subtests — and that red output went
into the implement handoff and the PR body as the proof the change was needed. The tester then
reproduced it independently by reverting `githubSkip` to `return false` uncommitted, watching the
same failure reappear verbatim, and `jj restore`-ing. Hold future test phases to that: break the
fix and see the reported symptom, not just re-run the green suite. #4315 did the same for its
tests: each failed on main's normalizer with the symptom production showed.

No recorded `merge_group` delivery existed when the test was written (the queue was not on yet),
so the fixture (`testdata/merge_group_checks_requested.json`) is production-shaped from GitHub's
documented payload and the test comment says so — the same convention as `sub_issues_added.json`.
Replace it with a captured delivery once one exists, but do not wait for one to write the test.

## The drop runs after signature verification, and one subtest pins that

The handler's order is method → body (25 MiB cap, GitHub's payload maximum: a larger declared length is 413 unread; each body's buffer is charged to a 64 MiB budget shared by every request as the body arrives, waiting when it is spent) → `X-GitHub-Delivery`/`X-GitHub-Event` →
`verify.Github(secret, body, signature)` (401) → JSON decode → sender log → CI observations →
envelopes → publish. An event `GithubEnvelopes` publishes nothing for is answered 200 `ok` at the
end of that order.

A drop that ran *before* verification would let anyone POST an unsigned `merge_group` and get 200.
Nothing in the code enforces the order, so `TestGitHubHandlerMergeGroup`'s third subtest is the
guard: the same fixture with `X-Hub-Signature-256: sha256=invalid` must answer 401 with nothing
published. Any new early return added to a webhook handler that verifies signatures gets the same
unsigned subtest.

## A new event is a deliberate act

To publish another GitHub event, give it a kind in `githubEventKinds`, a case in `githubPayload`
and `githubSummary`, and a body in `TestEveryPublishedGithubEnvelopeCarriesAJSONObjectPayload`.
Never bring back a default kind: it is how a commit status was filed as a comment, and
`TestGithubEnvelopesPublishNoEventTheListenerDoesNotNormalize` fails on it, as
`TestGithubEnvelopesPublishNoEnvelopeWithAnEmptyPayload` fails on a publish with no payload.

## The fixed listener is not autodeployed

A merge that touches `packages/envoy/**` runs `Release Envoy Listener`
(`.github/workflows/release-envoy-listener.yaml`, `on: push: branches: [main]` with that path
filter) and publishes `ghcr.io/cybermonklabs/legion/envoy:<merge sha>` and `:latest`. But
`packages/envoy/deploy/scripts/autodeploy.sh` runs `up -d --no-deps dispatch` only (lines 108 and
145): the **listener** container keeps the old image until the operator restarts it on the new tag.
Until then the pre-fix behaviour continues in production — one `merge_group checks_requested` and
one `merge_group destroyed` junk delivery per queued pull request on the repository's `comment`
topic. The implementer's post-merge production check therefore (a) names the image tag to the
operator session — only the operator touches the box — and (b) subscribes to
`notifications.github.<owner>.<repo>.comment` through one queue cycle and reports which build the
listener runs by whether the junk deliveries still arrive. If GitHub is not delivering
`merge_group` to the App at all (event subscription), neither appears in either case; say so
rather than reading silence as the fix.
