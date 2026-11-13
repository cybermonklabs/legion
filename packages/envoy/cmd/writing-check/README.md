# writing-check

`writing-check` takes the measurements of Dispatch's writing check (INFRA-3231) under the method
of INFRA-3189. It is a development tool. No image and no release ships it.

Every file it writes holds Dispatch text or model replies. Write them under `$TMPDIR`, never into
a repository.

```sh
go run ./cmd/writing-check probe-model --kind <kind> --endpoint <url> --deployment <id> [flags]
go run ./cmd/writing-check fetch --after <event id> --through <event id> --out <cases.jsonl> [--resume]
go run ./cmd/writing-check group --in <cases.jsonl> --out <groups.jsonl>
go run ./cmd/writing-check rule-cost --in <cases.jsonl> --seed N --out <dir> [--writes 500] [--min-per-surface 30] [--plant <rule id>]
go run ./cmd/writing-check split --in <grouped.jsonl> --cut-a <event id> --cut-b <event id> --out <dir>
go run ./cmd/writing-check plant --in <manifest.jsonl> --corpus <cases.jsonl> --seed N --out <dir> [--per-shape 40] [--nolist 40] [--controls]
go run ./cmd/writing-check label --in <cases.jsonl> --definition <file> --out <labels.jsonl> --endpoint <url> --key-file <file> [flags]
go run ./cmd/writing-check validate-labeller --labels <labels.jsonl> --planted <manifest.jsonl> --seed N --out <report.md>
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

## Writes and surface families

The subcommands below count by write. A write is one request: the cases that share an event id
and a source id. A planted copy of a write is a write of its own (its `copy` id tells it apart).

Each surface belongs to one family:

| Family | Surfaces |
| --- | --- |
| `comment` | `comment`, `comment_suggestion` |
| `comment_edit` | `comment_edit` |
| `message` | `message` |
| `delivery_reply` | `delivery_reply`, `comment_delivery_reply` |
| `send_to_agent` | `send_to_agent` |
| `broadcast` | `broadcast` |
| `ask` | `ask_question`, `ask_option` |
| `ask_answer` | `ask_answer` |
| `ask_resolution` | `ask_resolution`, `ask_decline` |
| `issue_title` | `issue_title` |
| `document` | `document` |
| `version_summary` | `version_summary` |
| `approval_summary` | `approval_summary` |
| `review_reason` | `review_reason` |
| `result` | `result_run`, `result_observed`, `result_negative_control` (one write) |

A write whose cases fall in two families is of the family listed first. So a document version
with its summary is a `document` write, and a version with only a summary is a
`version_summary` write.

A write's main prose field is the field a person reads first, in this order of surfaces:
`document`, `comment`, `comment_edit`, `comment_delivery_reply`, `message`, `delivery_reply`,
`send_to_agent`, `broadcast`, `ask_question`, `ask_answer`, `ask_decline`, `ask_resolution`,
`approval_summary`, `review_reason`, `result_observed`, `result_run`,
`result_negative_control`, `version_summary`, `comment_suggestion`, `ask_option`,
`issue_title`. Among several fields of one surface, the longest is the main field.

A planted sentence goes in as its own paragraph at a paragraph start of the main field. A
paragraph start is the start of the text (after front matter), the start of a line that follows
a blank line, opens with no white space and is outside a fenced code block, and the end of a text
that does not end inside a fence.

## rule-cost

`rule-cost` measures the share of writes that each structure rule (`writingcheck/rules.All()`)
finds, and picks the rules to enable.

| Flag | Meaning |
| --- | --- |
| `--in` | The cases file. |
| `--seed` | The seed of the draw. |
| `--writes` | The least number of writes to draw (default 500). |
| `--min-per-surface` | The least number of writes to draw from each family (default 30). |
| `--out` | The directory for the output files. |
| `--plant` | A rule id, for a negative control (below). |

The draw is stratified by family:

1. Each family gives `--min-per-surface` writes, or all its writes when it has fewer.
2. The rest of `--writes` is shared out by each family's count of writes in the input (largest
   remainder). No family gives more writes than it has. When the minimums take more than
   `--writes`, the draw is larger than `--writes`.
3. Each family's writes are drawn uniformly by the seed, without replacement.
4. Each drawn write of a family carries the weight (writes of the family in the input) / (writes
   of the family drawn). So the weighted draw has each family's share of the input.

The command fails when the draw holds writes of fewer than two projects.

A write breaks a rule when a prose sentence of any of its fields (`prose.ProseSentences`) gives
a finding. For each rule, the table gives the pooled share (the weighted share of drawn writes
that break it) and, for each family, the count of drawn writes that break it.

The enable rule: take the rules whose pooled share is at most 5%, in rising order of share (ties
in the order of `rules.All()`). Enable each in turn while the union share, the weighted share of
writes that break any enabled rule, stays at most 10%. Stop at the first rule that would take the
union share over 10%. The table also lists `rules.LeftOut` with the reasons.

`--plant <rule id>` puts one sentence written for this command, which breaks that rule and no
other, into the main field of every drawn write at a paragraph start drawn by the seed. That rule
must then read 100%, and it is not enabled.

Output, in `--out`:

- `table.md`, which the command also prints, and `table.json`, with the same numbers;
- `manifest.jsonl`: one line for each drawn write, with `event_id`, `source_id`, `family`,
  `project` and `weight`;
- `manifest.sha256`: the SHA-256 of `manifest.jsonl`, in the format of `sha256sum`.

## split

`split` cuts a grouped cases file (the output of `group`) into the three sets of the method:
development, calibration and final pool. Each group goes whole to the set of its first case (its
lowest event id):

- `development`: the first case is before `--cut-a`;
- `calibration`: the first case is at `--cut-a` or after it, and before `--cut-b`;
- `final-pool`: the first case is at `--cut-b` or after it.

Output, in `--out`: one manifest for each set (`development.jsonl`, `calibration.jsonl`,
`final-pool.jsonl`) and `counts.json`. Each manifest line is one case: `id`
(`<event id>/<source id>/<field>`), `event_id`, `source_id`, `field`, `group`, and the empty label
slot `label`, `label_source` and `label_revision` (all `null`). `counts.json` holds the cuts, the
count of cases, writes and groups of each set, and each manifest's SHA-256. The command prints
the counts.

The command fails on a case with no group, on a group in two sets and on a write in two sets.

## plant

`plant` copies real writes and plants one sentence into each copy. The planted copies are for the
labeller's validation. Each copy carries the tag `planted`, so no other eval reads it as real
work.

| Flag | Meaning |
| --- | --- |
| `--in` | A manifest that names the writes to copy: a set manifest of `split`, or the manifest of `rule-cost`. |
| `--corpus` | The cases file that holds the text of those writes. |
| `--seed` | The seed of every draw. |
| `--per-shape` | The faults of each named shape: `names`, `shows`, `pays` (default 40). |
| `--nolist` | The faults whose verb is not *name*, *show* or *pay* (default 40). |
| `--controls` | Plant one matched control for each fault. |
| `--out` | The directory for the output files. |

The sentences come from templates in `plant.go`. They are written for this command, with generic
subjects and objects, in the present, the past and the future:

- A fault states no fact for its verb and has no literal reading, for example "The new section
  names the risk." Its object is an abstract noun that states no value (the risk, the gap, the
  problem, the cause, the work and the like). Its subject is a text, a change or an approach,
  never a thing that literally displays, carries, holds or pays (a plan, a log, a page, an
  output, a table, a report, a queue). The pays faults have subjects that pay no money (a
  branch, a cache, a scan). The other verbs are *frame*, *capture*, *surface*, *anchor*, *land*,
  *carry*, *unblock*, *bridge*, *drive*, *own*, *close*, *track*, *signal*, *reflect*, *move*,
  *hold*, *shape* and *back*, used figuratively, taken in turn in a seeded order.
  `TestFaultsHaveNoLiteralReadingAndStateNoValue` checks the subjects and objects.
- A control uses the same verb in a sentence that states the fact (a value, a holder, a date, an
  amount or an observed event), or in its literal sense with a fact, as the definition's boundary
  cases allow. For example: "The config file names `us-west1` as the region."

No sentence is planted twice in one run.

The writes come from the writes the manifest names whose main field is not a one-line field
(`issue_title`, `ask_option`, `comment_suggestion`). The families are taken in turn, and each
family's writes in a seeded order, so every family with a body is drawn from. A fault goes in at a
paragraph start of the main field drawn by the seed. Its control goes into a second copy of the
same write at the same paragraph start. A paragraph start is used only when the prose reader reads
the planted sentence there as one sentence of the main field, which the field holds nowhere else.
When no start of a write is usable, the next write is taken.

Output, in `--out`:

- `planted.jsonl`: the copies, as cases (every field of each copied write), with `copy` (the copy
  id) and `tag` (`planted`). The main field's `text` and `sha256` are the planted copy's.
- `manifest.jsonl`: one line for each copy: `copy`, `pair` (a fault and its control share it),
  `tag`, `kind` (`fault` or `control`), `shape` (`names`, `shows`, `pays` or `other`), `verb`,
  `sentence`, `sentence_prose` (the sentence as the prose reader reads it, with `CODE` for inline
  code), `event_id`, `source_id`, `family`, `project`, `field`, `surface`, `position` (the byte
  offset of the sentence in the copied field), `boundary` and `boundaries` (which paragraph start
  was used, of how many), and `seed`.
- `sha256`: the SHA-256 of both files.

## label

`label` labels each sentence of each write with a model labeller.

| Flag | Meaning |
| --- | --- |
| `--in` | A cases file, or the `planted.jsonl` of `plant`. |
| `--titles` | A cases file to read issue titles from (default `--in`). For planted copies, give the corpus. |
| `--definition` | The definition file. The command sends it to the model as the rubric. Keep it out of the repository. |
| `--out` | The labels file. The command refuses a file that exists, unless `--resume` is given. |
| `--resume` | Keep the writes the labels file holds with no error, and label the others. |
| `--concurrency` | The calls in flight at once (default 4). |
| `--kind` | The endpoint's protocol (default `anthropic-messages`). |
| `--endpoint` | The model gateway's base URL. |
| `--key-file` | A file that holds the gateway's key. The command never prints it. |
| `--model` | The model id (default `claude-opus-5-5`). |
| `--effort` | The reasoning effort (default `off`, no thinking). |
| `--chunk` | The most sentences one call labels (default 60). |
| `--call-timeout` | The time bound of one model call (default 5m). |

For each write, the command numbers the prose sentences of all its fields (`prose.ProseSentences`)
`s1`, `s2` and so on. It sends one JSON input to `agentloop.Run`: the issue title the write's
issue had at the write's event (the last `issue_title` case of the issue at or before it), the
surface of the main field, each field with its raw text, and the numbered sentences. A second
text block asks for every id: "Label all N numbered sentences, s1 to sN, with one entry each."
The system prompt is an instruction in `label.go` (the text is data; label every numbered
sentence) followed by the definition file. The reply must match the schema
`{sentences: [{id, verdict, shape, rewrite, reason}]}`, with `verdict` one of `vacuous`,
`not_vacuous`, `unresolved` and `shape` one of `names`, `shows`, `pays`, `other`, `none`. A reply
that leaves out a sentence, labels one twice or labels one that was not sent is unusable.

Each call is bounded at `--call-timeout` and 16,000 output tokens. A write whose sentences are more than
`--chunk` is labelled in chunks of `--chunk` sentences, each sent with the two sentences before
and after it as context. A write whose raw text is more than 150,000 characters is sent without
the raw text. A call that fails with a 429, a 5xx or a network error is made again up to 5 times,
after 2 s, 4 s, 8 s, 16 s and 32 s. A call whose reply is unusable, malformed or cut at the output
bound is made again at once, up to 4 times. (The labeller model at times answers for the first
sentence alone; a new call answers for all of them.)

Each line of the labels file is one write: `event_id`, `source_id`, `copy`, `family`, the write
label, each sentence (`id`, `field`, `kind`, `text`, `verdict`, `shape`, `rewrite`, `reason`)
and each call (`chunk`, `sentences`, `raw_text`, `attempts`, `start`, the last attempt's
`latency_ms`, the served `model`, the tokens of all attempts, the `fault`, the `error`, and for an
unusable reply the reply itself in `reply` and `reply_text`). The write label is `failure` when
any sentence is vacuous, `unresolved` when none is and any is unresolved, `pass` otherwise, and
`error` when a call failed (the line then holds `error`, and `--resume` labels the write again).

At the end the command prints the count of write labels and of verdicts, the served model ids,
the call latency and the tokens.

## validate-labeller

`validate-labeller` checks the labeller against the planted cases. It reads only the planted
sentences. A fault must be labelled `vacuous` and a control `not_vacuous`.

| Flag | Meaning |
| --- | --- |
| `--labels` | The labels file of `label` over the planted copies. |
| `--planted` | The `manifest.jsonl` of `plant`. |
| `--seed` | The registered seed. The command fails when a planted case was drawn with another seed. |
| `--out` | The report, Markdown. A `.json` file with the same name holds the same numbers. |

A planted sentence's label is the label of the one sentence of the copy's planted field whose text
is the manifest's `sentence_prose`. A copy with no label, a copy whose label holds an error, and a
sentence not found once are unlabelled.

The report counts the agreeing, disagreeing and unlabelled cases for each class (fault, control)
and case type (family), for each class, for each case type and in total. Each row gives the exact
one-sided upper bound on the disagreement rate at 97.5% (Clopper-Pearson, the quantile of
Beta(x + 1, n − x), with gonum's `distuv.Beta`). The report lists the served model ids and every
disagreeing or unlabelled case.

The labeller passes only with at least 183 agreeing cases in total and no disagreeing and no
unlabelled case in any class and case type. 183 agreeing cases and none disagreeing bound the
disagreement at 2.0% at 97.5%. The command exits 0 on a pass and 1 on a failure.
