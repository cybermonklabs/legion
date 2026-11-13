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
go run ./cmd/writing-check recall --labels <labels.jsonl> [--planted <manifest.jsonl>] [--heldout-seed N --heldout-share 0.5] --out <dir>
go run ./cmd/writing-check digest [--root <module root>]
go run ./cmd/writing-check score --labels <labels.jsonl> [--planted <manifest.jsonl>] --stratum real|planted --out <dir> [--final-test --set-digest <sha> --seed N --exposure-log <file>] <reader flags>
go run ./cmd/writing-check reproduce --outputs <outputs.jsonl> --labels <labels.jsonl> [--planted <manifest.jsonl>] --figures <figures.json> [--bootstrap-seed N]
go run ./cmd/writing-check regress --cases <cases.jsonl> --repeat-bound <share> <reader flags>
go run ./cmd/writing-check reading --in <cases.jsonl> --after <event id> --through <event id> [--exclude <manifest>]... --out <dir> <reader flags>
go run ./cmd/writing-check validate --description <scanner.yaml> --exposure-log <file> [--manifests <dir>]
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
  branch, a cache, a scan). The other verbs are *frame*, *capture*, *surface*, *anchor*, *carry*,
  *unblock*, *bridge*, *drive*, *own*, *close*, *track*, *signal*, *reflect*, *move*, *hold* and
  *shape*, used figuratively, taken in turn in a seeded order. *land* and *back* were dropped:
  each has a literal reading a labeller takes for a fact.
  `TestFaultsHaveNoLiteralReadingAndStateNoValue` checks the subjects and objects.
- A control uses the same verb in a sentence that states the fact (a value, a holder, a date, an
  amount or an observed event), or in its literal sense with a fact, as the definition's boundary
  cases allow. Every control states a fact of software work (a file, a command, a test, a build,
  a config value, a count, a person or a date), so a reader of a software write cannot take its
  subject for a thing of another domain. For example: "The config file names `us-west1` as the
  region."

No sentence is planted twice in one run.

The writes come from the writes the manifest names whose main field is not a one-line field
(`issue_title`, `ask_option`, `comment_suggestion`). The families are taken in turn, and each
family's writes in a seeded order, so every family with a body is drawn from. A fault goes in at a
paragraph start of the main field drawn by the seed. Its control goes into a second copy of the
same write at the same paragraph start. A paragraph start is used only when the prose reader reads
the planted sentence there as one sentence of the main field, which the field holds nowhere else.

A planted sentence must not read as someone's relayed or quoted words, so these starts are not
drawn:

- the start right after a block quote, a paragraph that holds a quotation mark (`"`, `“`, `”`,
  `„`, `«`, `»`, `‘`), or a paragraph that ends in a colon;
- the start right before a block quote or a paragraph that holds a quotation mark;
- every start after a paragraph that attributes words to a person (`From X:`; *wrote*, *said*,
  *asked*, *replied*, *answered*; *writes*, *says*, *asks*, *replies* followed by a colon or a
  quotation mark), up to the next heading.

When no other start serves, the first start of the main field is used, and the manifest line holds
`next_to_relayed: true`. When no start of a write serves at all, the next write is taken.

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

## Reference labels and bounds

`recall`, `score` and `reproduce` read a labels file of `label` and, for planted copies, the
`manifest.jsonl` of `plant`. Each write is read once; where the file holds a write twice (after
`label --resume`), the last line wins.

- A sentence's reference verdict and shape are the labeller's. In a planted copy, the planted
  sentence (the one sentence of the planted field whose text is the manifest's `sentence_prose`)
  takes them from the manifest instead: a fault is `vacuous`, a control `not_vacuous`, each of the
  planted shape.
- A write's reference label is `failure` when it holds a reference-vacuous sentence, `unresolved`
  when it holds none and an unresolved one, `pass` otherwise, `not_applicable` when it holds no
  prose sentence, and `error` when the labeller failed or the planted sentence is not found once.
  The rates read only writes labelled `failure`, `unresolved` or `pass`.
- A copy with a `copy` id is in the planted stratum; every other write is in the real stratum.
- The no-list sentences are the reference-vacuous sentences of shape `other` (a main verb that is
  not *name*, *show* or *pay*). `--heldout-seed N --heldout-share S` splits them into a tuning half
  and a held-out half: a unit goes to the held-out half when the first 8 bytes of
  `sha256("<seed>\0<unit>")`, as a share of 2^64, fall under S. The unit of a planted fault is its
  verb (`verb:<verb>`), so no verb of the held-out half is tuned on; the unit of a labelled sentence
  is its write (`write:<event id>/<source id>[/<copy>]`). The split depends on the seed and the
  unit alone, so `recall` and `score` given the same seed and share draw the same halves.

Every bound is exact and one-sided (Clopper-Pearson, gonum's `distuv.Beta`): the upper bound on a
rate after x events in n trials at confidence c is the Beta(x + 1, n − x) quantile at c (1 when
x = n or n = 0), and the lower bound is the Beta(x, n − x + 1) quantile at 1 − c (0 when x = 0).
0 of 59 bounds a rate at 4.95% at 95%, and 0 of 183 at 1.99% at 97.5%.

## recall

`recall` measures the code stage alone: for every reference-vacuous sentence of a usable write,
is it a candidate? It runs `candidates.Find` once over each write's sentences (the labels hold
each sentence's field and text in order, which is all the code stage reads), once with the
bundle's lists and once with `Options{NoLists: true}`, and matches each candidate to its sentence
by field and text. Find answers its candidates strongest first, not in document order, so each
candidate takes the first sentence not yet taken that has its field and text.

| Flag | Meaning |
| --- | --- |
| `--labels` | The labels file. |
| `--planted` | Plant's manifest, when the labels hold planted copies. |
| `--heldout-seed`, `--heldout-share` | Split the no-list sentences (default share 0.5). Without a seed, nothing is split. |
| `--out` | The directory for `recall.json`, `recall.md`, and with a split `nolist-tuning.jsonl` and `nolist-heldout.jsonl`. |

The table gives, for each population (each named shape, the no-list sentences, each half, all
reference-vacuous sentences), the sentences, the candidates found, the recall and its exact 95%
lower bound, with the lists and without them. Below it: the candidate share of all sentences and
of all writes, with and without the lists, and whether R13 holds: deleting the lists does not
lower the recall over the held-out half (over all no-list sentences without a split). Only the
tuning half is for tuning; the held-out half is written to its own file so it is never tuned on.

## digest

`digest` prints the bundle's digest and its files. `--root` is the module root (default `.`, so
run it from `packages/envoy`). The files, in this order, as paths from the module root:

1. `internal/dispatch/writingcheck/prose/*.go`
2. `internal/dispatch/writingcheck/rules/*.go`
3. `internal/dispatch/writingcheck/candidates/*.go`
4. `internal/dispatch/writingcheck/reader/*.go`, then `reader/prompt.md`
5. `internal/dispatch/writingcheck/bundle.go`, `checker.go`, `actor.go`
6. `internal/dispatch/writingcheck/baseline/*.go`

Each `*.go` is the directory's Go files without `_test.go`, sorted by name. The digest is the
SHA-256 of the lines `<sha256 of the file>  <path>\n` in that order, which is what
`sha256sum <files in order> | sha256sum` prints from the module root.

## score

`score` runs the detector on every write of one stratum of a labels file as the server's check
does: the code stage over all the write's prose sentences, then one reader call over all its
candidates (the reader reads at most 24). The reader is `writingcheck/reader` with the bundle's
prompt and schema.

| Flag | Meaning |
| --- | --- |
| `--labels`, `--planted` | The labels file and plant's manifest (`--planted` is required for `--stratum planted`). |
| `--stratum` | `real` or `planted`. |
| `--out` | The directory for `outputs.jsonl`, `figures.json` and `figures.md`. |
| `--labeller-bound` | The labeller's validated disagreement bound at 97.5%, for the composed bound. |
| `--heldout-seed`, `--heldout-share` | The split `recall` used, for the held-out no-list recall. |
| `--bootstrap-seed`, `--bootstrap` | The seed (default 1) and resamples (default 1000) of the cluster bootstrap. |
| `--root` | The module root, for the bundle digest (default `.`). |
| `--final-test`, `--set-digest`, `--seed`, `--exposure-log` | Mark the run as the final test; see below. |
| `--kind`, `--endpoint`, `--deployment`, `--api-version`, `--azure-tenant`, `--azure-client-id`, `--federated-token-file`, `--scope`, `--key-file`, `--no-effort`, `--deadline` | The reader flags of `probe-model`. The server sends no effort (`writingcheck.ReaderNoEffort`), so give `--no-effort` to match it on `openai-chat`. |
| `--model-id` | The model id the reader pins (`reader.Config.BundleModelID`, default the bundle's). A reply from another model is `model_mismatch`. |
| `--concurrency` | The writes read at once (default 4). Each worker has its own reader, so each call's served model id is its own. |

Each line of `outputs.jsonl` is one write: `write` (`<event id>/<source id>[/<copy>]`), `stratum`,
`sentences`, `candidates` (`id`, the labels' `sentence_id`, `field`, `text`, `reason`), the reader's
`outcome` (`read`, `candidate_cap`, `reader_timeout`, `reader_error`, `reader_malformed`,
`reader_budget`, `model_mismatch`, or `not_called` with no candidate), its `verdicts` (`id`,
`vacuous`, `rewrite`, `reason`), the scanner `verdict`, `model_called`, `latency_ms`, the served
`model`, `tokens_estimated` and `error`.

The scanner's verdict on a write:

- `not_applicable`: the write holds no prose sentence;
- `failure`: the reader called a candidate vacuous;
- `no_failure`: no candidate, or the reader read every candidate and called none vacuous;
- `insufficient_evidence`: candidates past the cap went unread and none read was vacuous;
- `error`: every reader fault (a timeout, a provider error, a malformed reply, a spent budget, a
  model mismatch). An `error` counts as a missed failure.

The figures, for the scanner and, on the same writes, for the baseline (`baseline.Matches` on
each sentence; a write is refused when any sentence matches):

- **Write precision**: over the refused writes with a usable label, those labelled `failure` (they
  hold at least one reference-vacuous sentence; in a planted fault copy, the planted fault is
  one). `1 − precision` is given with its exact upper bounds at 95% and at 97.5%. A refused write
  labelled `unresolved` counts against precision.
- **Composed bound** (with `--labeller-bound`): the labeller's bound plus the scanner's 97.5%
  upper bound on `1 − write precision`, and whether it is at or below 5%.
- **Sentence precision**: over the sentences the reader called vacuous, those that are
  reference-vacuous, with `1 − precision`'s bounds, and a 95% percentile interval by a cluster
  bootstrap over the writes holding a refused sentence (PCG source seeded with the bootstrap
  seed). The bootstrap describes these cases alone and certifies nothing.
- **Recall**: over the reference-vacuous sentences of usable writes, those the reader called
  vacuous: for each named shape, for the no-list sentences, for the held-out half, and for all.
  **Write recall**: over the writes labelled `failure`, those refused. Each with its count and
  exact 95% lower bound.
- The shares of writes the scanner calls `not_applicable`, `insufficient_evidence` and `error`,
  and the refused writes with no usable label (left out of precision).
- Over the writes a model call was made for: latency p50 and p95 (nearest rank), tokens p95, and
  the ceilings, twice each p95. The reader's `Observe` gives no token counts, so a call's tokens
  are estimated at four bytes a token, as the reader's budget counts: the system prompt, the
  candidates as sent, and the verdicts as returned.
- The served model ids.

`figures.json` holds the run (`run_id`, `time`, `final_test`, `set_digest`, `seed`,
`bundle_digest`, `model_id`, `deployment`, `deadline`, and the SHA-256 of the labels, the planted
manifest and `outputs.jsonl`), the parameters (stratum, split, labeller bound, bootstrap seed and
resamples, and `decimals`, 4), and the figures. `figures.md` is the same as tables.

With `--final-test` (which needs `--set-digest`, `--seed`, `--exposure-log` and a `--model-id`),
the command refuses a set whose digest the exposure log already holds a final-test run for. It
appends one line to the log (`run_id`, `kind: final_test`, `time`, `set_digest`,
`bundle_digest`, `seed`, `model_id`, `stratum`) before it reads any write, so a run that stops
part way still spends the set.

## reproduce

`reproduce` computes every figure again from `outputs.jsonl`, the labels and the planted manifest,
with the parameters `figures.json` records, and calls no model. `--bootstrap-seed` replaces the
recorded seed. It fails (exit 1) and lists each difference when `outputs.jsonl` is not the file
the run stored (its SHA-256), a write's stored verdict does not follow from its outcome and
verdicts, a write of the stratum has no output, or a figure differs from `figures.json` by more
than half a unit of its fourth decimal.

## regress

`regress` is the model half of the regression suite: it runs the detector on development cases
written for the test and compares each named sentence's verdict with the expected one.

| Flag | Meaning |
| --- | --- |
| `--cases` | A JSONL file of `{"id", "body", "expect": [{"sentence", "vacuous"}]}`. The body is read as the field `body`. Each expected sentence must be a prose sentence of its body. The reader's development cases are `internal/dispatch/writingcheck/eval/regress-cases.jsonl`, and a test of that package checks that each of their expected sentences is a candidate of the code stage. |
| `--repeat-bound` | The repeatability bound: the largest share of each class that may disagree. |
| `--out` | A JSON file for the report. |
| reader flags | As `score`. |

A sentence is called vacuous when it is a candidate and the reader called it vacuous. A reader
fault disagrees on every expected sentence of its case. The command prints, for each class
(`vacuous`, `not_vacuous`), the expected sentences, the disagreements and their share, lists each
disagreement, and exits 1 when either share is above the bound.

## reading

`reading` takes R11's reading over a window of agent writes with the frozen bundle.

| Flag | Meaning |
| --- | --- |
| `--in` | The cases file of `fetch`. |
| `--after`, `--through` | The window: events after `--after`, through `--through`. |
| `--exclude` | A manifest whose writes are left out (repeatable): the set manifests of `split`, the manifest of `rule-cost`, plant's manifest. Each line's `event_id` and `source_id` name a write. |
| `--out` | The directory for the output files. |
| `--root` | The module root, for the bundle digest. |
| reader flags | As `score`. |

Every write of the window that no excluded manifest names is read; a planted copy never is. Each
is checked twice by `writingcheck.Checker`, as the server checks it: once with the structure rules
the bundle enables (`writingcheck.EnabledRules`), once with the detector alone.

The report gives the share of writes the detector refuses, the share the rules refuse, the share
either refuses, the share where the detector's outcome is a fault (anything but `refused` or
`clean`), the candidate share of writes and of sentences, and the share of the window's body
bytes inside block quotes and inside fenced code. A line walker counts those bytes: a fenced
block runs from its opening line (three backticks or tildes, after at most three spaces) through
its closing line or the end; a block quote is a line opening with `>` after at most three spaces,
and each non-blank line straight after one.

A window with no agent write outside the excluded manifests is reported as `no reading`, and its
report holds no share, never a zero.

Output, in `--out`: `manifest.jsonl` (`event_id`, `source_id`, `project`, `family` of each write
read) and `manifest.sha256`; `results.jsonl` (each write's sentences, candidates, detector
outcome, whether the rules refused it, and its bytes; no text); `report.json` and `report.md`,
with the window, the excluded manifests and their SHA-256, the projects, the manifest's SHA-256
and the bundle digest.

## validate

`validate` checks a description file (`internal/dispatch/writingcheck/eval/scanner.yaml`, valid
against `scanner.schema.json` beside it) against the method's rules for a gate.

| Flag | Meaning |
| --- | --- |
| `--description` | The description file. A field the schema does not name fails it. |
| `--exposure-log` | The exposure log `score --final-test` appends to. A log that does not exist holds no run. |
| `--manifests` | The directory the manifests' `file`s are in (default: the description's directory). |

A field whose figures wait for the freeze or the final test carries `pending: true` and a
`reason`; the command lists each such field and does not fail on it. It fails (exit 1) on:

- a required field that is absent, or not pending and without its figures, a pending field with
  no reason, fewer than five boundary cases, and no baseline;
- for triage or a gate, no error-analysis record, or a sample under 50 opportunities or under 20
  sessions or issues;
- a manifest whose file's SHA-256 is not its `sha256`, a case with no group, a group in two sets,
  and a case whose evidence comes after its cutoff (`evidence_event_id` above `cutoff_event_id`,
  each the case's `event_id` where absent);
- before the claim (`final_test` pending), any final-test run of the final-test set in the
  exposure log; after it, no such run, more than one, one naming another bundle digest than
  `bundle_digest`, or another run id than `final_test.run_id`;
- an acceptance bar committed after the final-test run: `acceptance_bar.bar_committed_at`, or
  else the committer time of the `acceptance_bar:` line by `git blame` (a line not committed
  fails), against the run's time in the exposure log;
- a final test with no baseline figures;
- for a gate, for each error (1 − precision with e = 1 − `precision_floor`, 1 − recall with
  e = 1 − `recall_floor`): fewer draws than ln(α) / ln(1 − e), an exact upper bound above e at the
  bar's confidence, and, with a validated labeller, the composed bound (the labeller's bound plus
  the scanner's, at 1 − α/2) above e.

`TestValidateRefusesAGroupInTwoSetsAndNoBaseline` is the negative control: a copy of a valid
description with one group in two sets and the baseline taken out is refused for both.
