# Fixtures captured from a running Dispatch

Each file here was captured from a deployment's Dispatch on 2026-11-26, then rewritten for this public repository.
Every text is replaced by neutral words, every issue key by `ACME-<n>`, and every id, session, agent, host and path by a neutral one.
Each shape is kept as captured: the keys and their order, the nesting, lists of strings, empty lists, lists of objects, and keys that hold spaces.

## `handoff-bodies.json`

Handoffs of two issues, one of each shape their bodies held, each as `GET /api/v1/handoffs/{id}` answers it: the list's fields and `body`, the text as stored.
The shapes are `{head}`; `{head, note}`; a test handoff `{head, implementerProof: {verdict, how}, failures: [], failed}`; a review handoff whose `keyFindings` is a list of objects; an implement handoff with lists of strings, an empty list, a list of objects and keys that hold spaces; and a plan handoff with an object inside an object.
`HistoryHandoff.test.tsx` draws each of them (row H5).

The capture, for each issue:

```
curl -fsS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<KEY>/handoffs" | jq -r '.[].id'
curl -fsS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/handoffs/<id>" | jq '.body | fromjson'
```

## `step-with-report.json`

The first run of an issue's steps, which names the handoff it reported in `report`, with all the members the record held: `id`, `issue`, `task`, `step`, `kind`, `role`, `type`, `agent`, `session`, `model`, `generation`, `attempt`, `started_at`, `ended_at`, `result`, `returned_to`, `report`, `rating` and `rating_reason`.
`history-model.test.ts` places it in the history with the handoff it reports (row M3).

The capture:

```
curl -fsS -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/issues/<KEY>/steps" | jq '.tasks[0].steps[0]'
```
