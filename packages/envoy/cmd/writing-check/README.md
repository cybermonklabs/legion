# writing-check

`writing-check` takes the measurements of Dispatch's writing check (INFRA-3231) under the method
of INFRA-3189. It is a development tool. No image and no release ships it.

Every file it writes holds Dispatch text or model replies. Write them under `$TMPDIR`, never into
a repository.

```sh
go run ./cmd/writing-check probe-model --kind <kind> --endpoint <url> --deployment <id> [flags]
go run ./cmd/writing-check fetch --after <event id> --through <event id> --out <cases.jsonl> [--resume]
go run ./cmd/writing-check group --in <cases.jsonl> --out <groups.jsonl>
```

Run it from `packages/envoy`.

## probe-model

`probe-model` times the model reader's calls against one model endpoint. Each call goes through
the real reader (`writingcheck/reader`: `reader.New`, then `Read`). The candidates come from the
real code stage (`writingcheck/candidates`) over one body. So the prompt, the reply schema and the
bounds are the bundle's.

| Flag | Meaning |
| --- | --- |
| `--kind` | `openai-chat` for an Azure AI Foundry deployment, `anthropic-messages` for a model gateway. |
| `--endpoint` | The endpoint's base URL. |
| `--deployment` | The model id, or the Azure deployment name. |
| `--api-version` | The `api-version` query parameter, for an Azure deployment. |
| `--azure-tenant`, `--azure-client-id`, `--federated-token-file`, `--scope` | Keyless Azure sign-in through `embed.NewEntraTokenSource`, as the answerer signs in. `--scope` defaults to `embed.DefaultEntraScope`. |
| `--key-file` | A file that holds the endpoint's key. `openai-chat` sends it as a bearer token, `anthropic-messages` as `X-Api-Key`. The command never prints it. |
| `--calls` | The number of calls (default 20). |
| `--deadline` | The reader's time bound for one call (default 2s, the bundle's). |
| `--bodies` | A JSONL file of `{"text": ...}` bodies. Without it, the command uses a built-in set of synthetic sentences. A body that holds no candidate is not sent. |
| `--out` | A JSONL file for the per-call records. |
| `--no-effort` | Send no `reasoning_effort` (`openai-chat` only), for a deployment that refuses the parameter. Without it, the reader asks for effort `off`, which Chat Completions receives as `reasoning_effort: none`. |

Give either `--key-file` or the Azure sign-in flags.

Each record holds the call's start time, its latency in milliseconds, the reader's outcome (`read`
for a call that read every candidate, else `reader_timeout`, `reader_error`, `reader_malformed`
and the rest), the model id the provider said served the call (empty when no reply came), the
error text, and the number of candidates sent.

The summary prints p50, p95 and the maximum latency (nearest rank), the share of calls over the
deadline or failed, the count of parse failures (`reader_malformed`), the count of each outcome
and of each served model id, and the window the calls ran in.

## fetch

`fetch` pages the event log (`GET /api/v1/events/log`, 500 events a page, oldest first) from the
event after `--after` through `--through`. It writes one case for each prose field of each write
that the actor rule checks.

The actor rule is `writingcheck.Checks`, the function the server calls:

- A `session` actor is checked, except a daemon session (`legion-daemon:<PROJECT>`).
- A `user` actor is checked only when its act names a session (`act.session`) that is not a
  daemon session.
- A `system` actor, and a person who wrote for themselves, are not checked.

`fetch` counts the writes the rule exempts, and prints them on stderr by exemption
(`exempt_person`, `exempt_daemon`, `exempt_system`) and by event type. It reads nothing else for
an exempted write.

Each case holds these fields:

| Field | Meaning |
| --- | --- |
| `event_id`, `event_type`, `time` | The event, and when it was written. |
| `project`, `issue` | Where it was written; `issue` is empty for a project document or a direct message. |
| `root_issue` | The top of the issue's ancestors, or the issue itself when it has no parent. An issue event names the ancestors itself. For other events, `fetch` reads each issue's parent once (`GET /api/v1/issues/{key}`) and keeps it. |
| `document`, `document_name` | The document's artifact id and name, when the text is on a document. |
| `source_id` | The comment, message, ask or result id; the broadcast id; or `<artifact id>@<version>`. |
| `surface` | The kind of write, from the table below. |
| `field` | The JSON path of the text in the payload, such as `body`, `options[1].label` or `resolution.reason`. |
| `actor_kind`, `actor_id`, `act_session` | The event's actor, and the session its act names. |
| `text`, `sha256` | The text, and the SHA-256 of its UTF-8 bytes. |
| `cut` | `true` when Dispatch would not serve the whole text (deleted since, or not readable with this credential), so the case holds the part the event carried. |

An event payload cuts a comment's or a message's body at 32,000 UTF-16 units
(`body_truncated`). `fetch` then reads the whole body by its id.

`fetch` makes at most 20 requests a second. It retries a 5xx, a 429 or a network error up to 5
times, waiting 0.5s, 1s, 2s, 4s and 8s.

At the end it prints the count of cases by surface on stdout. On stderr it prints the exempted
writes, and by surface the fields Dispatch would not serve whole.

`--resume` continues a cases file that an earlier run did not finish. It drops a line cut short,
and the lines of the last event the file holds, because that event's cases may be incomplete.
Then it reads again from that event. Each event's cases are written in one write.

`DISPATCH_URL` names the Dispatch server. `DISPATCH_TOKEN_FILE` names the file that holds the
bearer token. The command reads the token file on every request and never prints the token.

### Prose surfaces

Each row names a surface, the event that carries it, the route that writes it, and the reads
that enumerate it. Every surface is enumerated by `GET /api/v1/events/log`; the last column names
the extra read, where there is one.

| Surface | Event and field | Route that writes it | Extra read |
| --- | --- | --- | --- |
| `comment` | `comment.created` `body` | `POST /api/v1/issues/{key}/comments`, `POST /api/v1/artifacts/{id}/comments` | `GET /api/v1/comments/{id}` when the body is cut |
| `comment_suggestion` | `comment.created` `suggestion.replace_with` | the same routes, with a suggestion (its `body` is the reason, a `comment` case) | none |
| `comment_edit` | `comment.edited` `body` | `PATCH /api/v1/comments/{id}` | `GET /api/v1/comments/{id}` when the body is cut |
| `comment_delivery_reply` | `comment.answered` `body` | `POST /api/v1/comments/{id}/reply` | `GET /api/v1/comments/{id}` when the body is cut |
| `message` | `message.created` `body`, with no target and no broadcast | `POST /api/v1/issues/{key}/messages` | `GET /api/v1/messages/{id}` when the body is cut |
| `send_to_agent` | `message.created` `body`, with a `target` | `POST /api/v1/issues/{key}/messages` with a target, `POST /api/v1/agents/{session_id}/messages` | as `message` |
| `broadcast` | `message.created` `body`, with a `broadcast_id` | `POST /api/v1/broadcasts`; one case for each broadcast, not one for each recipient's copy | as `message` |
| `delivery_reply` | `message.answered` `body` | `POST /api/v1/messages/{id}/reply`, and a message posted with `in_reply_to` | as `message` |
| `ask_question` | `ask.opened` and `ask.edited` `question` | `POST /api/v1/issues/{key}/asks`, `POST /api/v1/artifacts/{id}/asks`, `PATCH /api/v1/asks/{id}`, and a document's `:::ask` block | none |
| `ask_option` | `ask.opened` and `ask.edited` `options[i].label`, `options[i].description` | the same routes | none |
| `approval_summary` | `ask.opened` and `ask.edited` of an approval ask: the requester's summary after Dispatch's `Approve <documents>?` | `POST /api/v1/artifacts/{id}/approval-requests` | none |
| `ask_answer` | `ask.answered` `answer.text` | `POST /api/v1/asks/{id}/answer` | none |
| `ask_decline` | `ask.resolved` `resolution.reason` of kind `declined` | `POST /api/v1/asks/{id}/decline` | none |
| `ask_resolution` | `ask.resolved` `resolution.reason` of kind `retracted` or `resolved` | `POST /api/v1/asks/{id}/resolve` | none |
| `issue_title` | `issue.created` `title`; `issue.updated` `title` when `changed` names it | `POST /api/v1/issues`, `PATCH /api/v1/issues/{key}` | none |
| `document` | `artifact.created` of a `doc` artifact; `artifact.version` of a version with no MIME type (a document's) | `POST /api/v1/issues/{key}/artifacts`, `POST /api/v1/projects/{key}/artifacts`, the `.../edits` routes, live editing | `GET /api/v1/artifacts/{id}/versions/{n}`, and version n−1 when the run has not read it |
| `version_summary` | `artifact.created` and `artifact.version` `version.summary` | the `.../versions` routes (a named version), an upload with a summary | none |
| `review_reason` | `artifact.approved` and `artifact.changes_requested` `reason` | `POST /api/v1/artifacts/{id}/reviews`, an answer to an approval ask | none |
| `result_run`, `result_observed`, `result_negative_control` | `result.recorded`, by `result_id` | `POST /api/v1/issues/{key}/results` | `GET /api/v1/issues/{check_issue}/results`, kept for each issue and read again only for a result it does not hold yet (Dispatch has no read of one result by its id) |

These rules decide what a case holds:

- An `ask.edited` case holds only the wording that changed from the `previous` wording the event
  carries. An option's label or description is new when no option before the edit had that text.
- An approval ask's question is Dispatch's own words up to the `?` after the last document named.
  Its options are Dispatch's too. Only the requester's summary is a case.
- A document version's case holds only the blocks that the version before it does not hold. The
  text is split at blank lines. A fenced code block is one block, whatever blank lines it holds.
  A list is split into its items, and a table into its rows; a row is compared with its table's
  head (its first two lines). The blocks are compared as sets, so a block that moved is not new.
  The new blocks are joined by blank lines, and the new rows of one table are written under its
  head once.
- `message.answered` carries `reply_body`, the first 160 characters of the message it answers.
  That is the other writer's text, so it is not a case.

These events carry no prose that an agent writes, and `fetch` skips them:

- `handoff.recorded`;
- `artifact.human_edited` (a person's edits);
- `suggestion.accepted`, `suggestion.rejected`, `comment.resolved` and `comment.reopened`;
- an `ask.resolved` of kind `withdrawn` (Dispatch's own words when the issue closes);
- the delivery, claim, follower, state and anchor events.

## group

`group` gives each case a `group`. A set split must keep every case of one group on one side.
Two cases are in one group when a chain of these links joins them (union-find):

- the same root issue (the issue itself where the case names no root);
- the same document (every version of it, and the comments and asks on it);
- near copies: their sets of word 5-grams have a Jaccard similarity of 0.9 or more.

A word is a run of letters and digits, lower-cased. A text of fewer than five words is one
shingle of all its words. A shingle is kept as its 64-bit FNV-1a hash.

The near-copy join is exact: it finds every pair that comparing all pairs finds. It uses prefix
filtering (the all-pairs similarity join):

1. Cases with the same shingle set are joined at once, and only one of them goes on.
2. Each set's shingles are ordered rarest first, over all cases.
3. If two sets of sizes m ≤ n have a Jaccard similarity of t or more, they share at least ⌈t·n⌉
   shingles. So they share a shingle among the first n − ⌈t·n⌉ + 1 shingles of each, and the
   smaller set has at least ⌈t·n⌉ shingles.
4. Sets are taken shortest first. Each set looks up its prefix in an index of the prefixes taken
   before it, skips the candidates that are too short, and computes each other candidate's exact
   Jaccard similarity. Then its own prefix joins the index.

`TestNearCopiesMatchesEveryPairCompared` checks this join against a comparison of every pair, on
synthetic texts at thresholds 0.9, 0.75 and 0.5.

Group ids are `g1`, `g2` and so on, in the order of each group's first case. `group` prints the
count of cases and the count of groups.
