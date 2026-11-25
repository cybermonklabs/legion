# Dispatch scripts

## Ask census

`ask-census.ts` reproduces the question and approval-request counting used for LEGION-470. It
reads every issue updated since the window opened and each issue's asks, then every page of the
events of each issue that carries an approval ask, whenever that ask was opened. Eight issues are
read at a time, each through its own Dispatch client, so each issue's reads get the client's
60-second deadline (`DISPATCH_TOOL_DEADLINE_MS`) to themselves and a run may take as long as it
needs.

```bash
bun scripts/dispatch/ask-census.ts \
  --from 2026-10-01T03:40:00Z \
  --to 2026-10-02T02:10:00Z \
  --project ACME \
  --project LEGION
```

`--project` is required and repeats: name each Dispatch project the census reads.

The script resolves Dispatch as every command does, with `sessionDispatchSettings`
(`packages/envoy-client/src/session-settings.ts`) over the envoy.json harness `command` reads: the
file `ENVOY_CONFIG_FILE` names, else the omp profile's or `CLAUDE_CONFIG_DIR`'s (only under
`CLAUDE_CODE_CHILD_SESSION=1`, as in a Claude Code shell), else `~/.config/legion/envoy.json`: the
URL from `DISPATCH_URL`, else `dispatch.serverUrl` with `dispatch.enabled`; the token from
`DISPATCH_TOKEN_FILE`, else `DISPATCH_TOKEN`, else `dispatch.token`. A checkout's
`.opencode/envoy.json`, which is not read, is named on stderr with its rename while the checkout
has no `.envoy/envoy.json`. With Dispatch off it stops with `Dispatch is not configured: <notice>`,
the notice saying which file and why, or `dispatch config: <error>`. It prints total asks, decision
blocks, standalone question asks, approval requests, per-issue counts, approval rounds, every
standalone question with a blank `code` column, and a per-session table.

A session still running an old plugin can be removed from the measurement with a repeated
`--exclude-session <id>`. The summary prints how many asks were excluded.

`--json` prints the same numbers as one JSON object on stdout and nothing else — the ask totals,
the per-issue counts, the approval rounds, the coded standalone questions, the coding totals, the
excluded-ask count and the per-session rows — so one day's numbers can be compared with another's.

Classify each standalone question in a CSV passed with `--codes`:

```text
# ask-id,code
4d0b86f4-6a98-4199-b91d-45830f6a1d1b,design
34edccbb-4b4e-4e7d-9d81-2fa7d8e4198b,to-do
```

The allowed codes are `to-do`, `design`, `may-I-proceed`, and `operations`. A `to-do` needs an
action or permission only a human can complete. `design` is a decision about the issue document.
`may-I-proceed` asks permission for work already requested. `operations` is a people or operations
decision with no design document. The script reports uncoded standalone questions separately.

For every document with approval activity in the window, **Approval rounds** shows Inbox rows,
arrivals, and human turns. An arrival is the request reaching the human's Inbox: each `ask.opened`,
and each `ask.handed_back`. An `ask.edited` only rewords a request (a move to a new version, a new
summary) and never arrives; a hand-back with a new summary is an `ask.edited` followed by its
`ask.handed_back`, one arrival. Before #1671, when an approval request began following its
document's versions, a request made again opened a new row, so there each arrival is an
`ask.opened`. A human turn is an answer to the request or a reply in its thread,
and, from the round's first request on, an answer or reply on a decision block in the document
the request names: a choice the human raises while the request is open becomes such a block, and
the hand-back after its answer responds to that turn. A round with more arrivals than human turns
plus one is flagged. `scripts/e2e/lib/design-gate-approval-requests.jq` reads the same arrivals
for the stage 4b proof.

## Display read census

`display-read-census.ts` reads every stored text of a Dispatch deployment through its API and
asks, text by text, whether Go's display read (`pmdoc.ReadForDisplay`) says what the browser's own
engine says. It is the measurement INFRA-635 rests on, and the recipe is INFRA-172's
(`docs/solutions/envoy/a-change-to-how-dispatch-reads-stored-markdown-censuses-every-stored-version-and-rewrites-the-hits-before-the-merge-INFRA-172.md`).

```bash
bun scripts/dispatch/display-read-census.ts --fetch-only   # hours: read production
bun scripts/dispatch/display-read-census.ts --no-fetch      # minutes: class what was read
bun scripts/dispatch/display-read-census.ts --no-fetch --json
```

It reads comments, the messages on issues, each ask's question, option labels and descriptions,
answer text and resolution reason, each document's approval reason, each architecture component's
prose, and every version of every document. Dispatch is resolved as for the ask census, with
`sessionDispatchSettings`, so it reads with the session's own agent token.

Three rules of the recipe are in the script and not negotiable: a version is read with
`GET /api/v1/artifacts/{id}/versions/{n}`, never `/text`, which renders the live tree again; each
project's issues come from one unpaged `GET /api/v1/issues?project=`, because offset paging
repeats and skips issues while others write; and a network failure or a 5xx is retried. It sends
**at most five requests a second**, eight in flight, so a pass over a deployment of some three
thousand issues takes between one and a half and two hours.

The corpus is written under `$TMPDIR` (`--work`, `--corpus`) and **is never committed**: it is
production's prose. It doubles as the resume file. Each unit of work - one list, one version - is
appended as one JSONL line as it lands, and a pass started again skips every unit already in it,
so an outage an hour in costs the hour and not the pass. INFRA-172's first pass lost 689 requests
to an outage and was discarded.

Both readings come from the readers themselves, through the pmdoc differential's harnesses
(`packages/envoy/internal/dispatch/pmdoc/gen/differential.ts` and that package's
`TestDifferentialDump`): a document version is held to the editing engine in `DisplayVersion`, and
every other stored text to the read-only engine in `DisplayLatest`, which is the pairing the
differential's own cross-reader gates use. Trees are compared with their block ids ignored.

Classing is the long half of a run - hours over a hundred thousand texts on two cores, where
reading them takes one - so it is done once. Every text classed is appended to `verdicts.jsonl`
in the work directory, each run reads only the texts no run has classed, and the readings of each
such batch stay in a `batch-<time>/` directory of their own. A pass over a corpus that grew by a
quarter therefore costs a quarter of the classing. Delete `verdicts.jsonl` to class them all
again, which is what a change to either reader, or to the classes, calls for; the batch
directories hold the readings and can be deleted whenever the disk is wanted back.

Each text falls in one class, defined in the script's opening comment: **the two agree**, **format
differs** (one visible text, two trees), **the browser lost text** (the browser's visible text is
Go's with characters deleted), **Go loses text** (the other way round, which must never happen),
**the text differs both ways**, **the browser cannot read it** (the engine threw) and **Go cannot
read it**. A *group* is where the text is stored. The report gives every class, its split by
group, every group's total, the requests the pass made and how long it took.

`--check-no-stored-text FILE...` is the guard that keeps production's words out of the repository.
It refuses a file that holds, run together with its whitespace collapsed, the first 64 characters
of any paragraph of any text the census read, and names the text it came from; it exits 1 on a hit
and 0 on none. It reads the corpus and nothing else, so it needs a pass to have been run.

`__tests__/display-read-census.test.ts` holds the classing to
`__fixtures__/display-read-census.json`: ten texts written for the purpose, eight of them with the
trees this repository's own two readers made of them, and the two pairs no text produced written
by hand, plus the stored texts and files the guard is tested on.

The fixture is generated, not written by hand: `/legion/scratch/tmp/drc-probe/fixture.ts` on the
pod that ran the census holds the texts and calls `classifyCorpus` and `classifyBatch`, then
writes the JSON; `bun fixture.ts` followed by `bunx biome check --write scripts/dispatch` gives
back the committed file byte for byte. It stays scratch and out of the tree; its whole input is
the fixture's own `markdown` and `why` fields, so it can be written again from them.

## Comment delivery

`comment-delivery/report.ts` answers which comments people wrote in Dispatch reached no agent
session, and why (sjawhar/legion#2366). The run has three steps. Every step only reads.

1. `comments.sql` reads Dispatch's database and writes `comments.csv`: every comment that a person
   wrote in the window, one row for each session or role that the comment was owed or sent to (a
   pair). `trace.sql` writes Dispatch's whole record of one comment. Both are in
   `packages/envoy/internal/dispatch/deliveryaudit/`. Its `README.md` gives their columns.
2. `tq receipts` (in example-org/deployment `transcript-store/`) reads
   `comments.csv` and the transcript store. It writes `receipts.tsv`, one status for each pair,
   and `receipts-counts.txt`, its counts. It owns the transcript rules: which transcript row is a
   receipt of a comment, which sessions count as the pair's session, and each pair's status.
3. `report.ts` reads the three files and writes one markdown document and one TSV file of the
   document's pairs.

example-org/deployment's `transcript-store/comment-delivery/run.sh` does steps 1 and 2 on a dev box, which
reaches both Dispatch's database and the transcript store.

### The run

Use one window for both steps. That run chooses `TO`. Both times are UTC, in the form
`2026-10-01T00:00:00Z`. Each psql run is one read-only transaction, so a mistake cannot write, and
`-q` keeps psql's `SET` line out of the CSV.

```bash
FROM=2026-10-01T00:00:00Z
TO=2026-10-27T05:00:00Z

# 1. Dispatch: every person's comment in the window, one row per pair.
psql "$DATABASE_URL" --single-transaction -q -X -v ON_ERROR_STOP=1 --csv \
  -c 'SET TRANSACTION READ ONLY' \
  -v from="$FROM" -v to="$TO" \
  -f packages/envoy/internal/dispatch/deliveryaudit/comments.sql > comments.csv

# Optional: Dispatch's whole record of one comment, once for each comment to show.
psql "$DATABASE_URL" --single-transaction -q -X -v ON_ERROR_STOP=1 --csv \
  -c 'SET TRANSACTION READ ONLY' \
  -v comment=03207432-749f-4211-9607-e378dc636807 \
  -f packages/envoy/internal/dispatch/deliveryaudit/trace.sql > trace-03207432.csv

# 2. The transcript store (example-org/deployment transcript-store/): receipts.tsv on stdout, the counts on stderr.
tq receipts --comments comments.csv --from "$FROM" > receipts.tsv 2> receipts-counts.txt

# 3. The document, and the TSV file of its pairs.
bun scripts/dispatch/comment-delivery/report.ts \
  --comments comments.csv --receipts receipts.tsv --counts receipts-counts.txt \
  --findings findings-bet-15.md --trace trace-03207432.csv \
  --pairs human-comment-delivery-2026-10-pairs.tsv > human-comment-delivery-2026-10.md
```

`--pairs` is required. `report.ts` writes the rows of the pairs to that file, and not to the
document, because Dispatch refuses a Markdown document with too many elements; upload the file as
an artifact beside the document. The file is TSV with a header line and one row for each pair, in
the order of the document's comments. Its columns are `comment_id`, `target`, `owed_because`,
`sent_by_the_outbox`, `mention_attempts`, `checked_session`, `status`, `class` and `cause`. A
backslash, a tab, a carriage return and a newline in a value are written as `\\`, `\t`, `\r` and
`\n`. The document's "Each pair" section gives the file's base name and its number of rows.

`--findings` and `--trace` can each be given more than one time. The document holds each findings
file whole, as Markdown, in the order given, in its "Findings" section after the table of causes
and their fixes. Start the headings of a findings file at `###`, because the section is a `##`
heading. The document holds each trace file whole, in a code block under the file's name, in the
order given. The same files give the same document and the same pairs file, byte for byte.

`report.ts` reads `receipts-counts.txt` as exactly these 15 lines, in this order. Each line is a
key, a tab and a value, with no header. `from` and `to` are UTC times in the form
`2026-10-01T00:00:00Z`. Every other value is a plain integer. The file ends with a newline.

```text
from	<UTC time>
to	<UTC time>
listed_pairs	<N>
received	<N>
not_received	<N>
unknown	<N>
unknown_session_not_in_store	<N>
unknown_role	<N>
unknown_btw	<N>
unlisted_rows	<N>
comments	<N>
comments_with_receipt	<N>
comments_without_receipt	<N>
empty_target_pairs	<N>
rows_without_id	<N>
```

The document gives `from` and `to` as its window. `report.ts` counts each number again from
`comments.csv` and `receipts.tsv`, and it checks these rules:

- `listed_pairs` = `received` + `not_received` + `unknown`, the listed rows of `receipts.tsv`.
- `unknown` = `unknown_session_not_in_store` + `unknown_role` + `unknown_btw`.
- `unlisted_rows` is the number of rows of `receipts.tsv` with `listed` `false`.
- `comments` = `comments_with_receipt` + `comments_without_receipt`: the distinct `comment_id`s
  of `comments.csv`, also those of a comment with no target. A comment "with receipt" has a row
  in `receipts.tsv`, listed or unlisted, with the status `received`. Its evidence can be
  `transcript` or `btw_reply:<id>`.
- `empty_target_pairs` is the number of rows of `comments.csv` with no target. Such a row has no
  row in `receipts.tsv`, so `listed_pairs` = the rows of `comments.csv` - `empty_target_pairs`.
- `rows_without_id` is 0. It counts the rows of a person's comment in the transcripts from which
  `tq receipts` read no id. When it is not 0, `receipts.tsv` can miss receipts.

`receipts.tsv` has its rows sorted by `comment_id`, then `listed` (`true` first), then `target`.
`listed` is `true` or `false`. A time is in the form `2026-10-20T04:00:01.000Z`, or empty. An
unlisted row has the target `session:<base session id>`: the id of the session that its forks
come from. Its `checked_session` can be empty.

### Classes

A pair is judged from its status in `receipts.tsv`:

- `received`: delivered. The evidence is `transcript` (a transcript has the comment), or
  `btw_reply:<id>` (the session's BTW side turn posted that reply to the comment).
- `unknown`: unknown, with the reason that `tq receipts` gives.
- `not_received`: not delivered, with the first cause that applies, in this order:
  1. `never-published`: Dispatch's outbox never published the comment's event.
  2. `not-addressed:<rule>`: the pair was owed the comment, the outbox did not send it to the
     pair (`addressed` is `false`), and no delivery of a mention to the pair was sent, queued or
     failed. The rule says why the pair was owed it: `claimant`, `route`, `follower`, `asker` or
     `mention`, joined with `+`.
  3. `mention-failed:<error>`: the newest attempt of a mention to the pair failed. The error is
     `session-not-live`, `receipt-timeout` or `other`.
  4. `mention-queued`: the newest attempt of a mention to the pair is `queued`. The Envoy
     listener took the mention and stored it for the session, which it knows and which was not
     running; the session reads it when it runs again, within the 72 hours the stream keeps it.
  5. `not-running`: the comment's `published_at` is not between the session's
     `target_started_at` and `target_ended_at`. These are the transcript store's recorded start
     and end of the session's transcript file, not the times that the session's process was up.
     An empty `target_ended_at` means that the store records no end for the file.
  6. `running-no-row`: any other `not_received` pair. Dispatch sent the comment to the session
     within the store's recorded start and end of the session's file, and the transcript does
     not have it. The process can still have been down inside that span; the loss is in the last
     hop or in the plugin.

`report.ts` puts each comment in one class. It checks the classes in this order and uses the
first that applies:

1. **Delivered.** A row of the comment in `receipts.tsv` has the status `received`. The row can be
   a pair or an unlisted row. An unlisted row is a session that took the comment in and is not a
   pair's session or a session forked from one.
2. **Not delivered, `never-published`.** Dispatch's outbox never published the event.
3. **Not delivered, `no-target`.** No session was owed the comment, and Dispatch sent it to no
   session or role.
4. **Unknown.** A pair of the comment is unknown. The document names each such pair and its
   reason.
5. **Not delivered, with one cause.** The first cause of its pairs, in the order above.

### What unknown means

An unknown pair is one that the report cannot judge. The transcript store cannot show whether the
pair got the comment. The reason is one of these:

- `session_not_in_store`: the store does not hold the session. The store holds the Oh My Pi
  sessions of the dev boxes and of the Legion pods, and no Claude Code session of either. It loads
  each machine on its own schedule, so a session newer than its machine's last load is not in it
  yet.
- `role`: the pair is a role, and no mention of the role resolved to a session. No session's
  transcript can show the receipt.
- `btw`: Dispatch delivered a mention as a BTW, and has no reply from the side turn. A BTW side
  turn adds nothing to the transcript.

An unknown is never counted as delivered or as not delivered.

### The document and its refusals

The document gives the counts of comments and of pairs with the identity
`total = delivered + not delivered by cause + unknown`. `CAUSES` in `classes.ts` maps each cause to
its fix. The document has a row for each comment, the pairs file has a row for each pair, and the
document lists the unlisted rows. No session was owed a comment with the cause `no-target`; give
the cause of each such comment in a findings file.

The document writes each value that it copies from its input (an owner, an author, a target, an
evidence, a receipt's place, a file name) as a code span, so that a Markdown editor does not make
an e-mail address or a URL in it a link and the uploaded document stays byte for byte the one a
re-run writes. A comment id of only hex digits and dashes stays bare. A findings file and a trace
go in as they are.

The report refuses its input (exit 1, each problem on stderr, nothing on stdout, no pairs file) in
these cases:

- A file that `--comments`, `--receipts`, `--counts`, `--findings` or `--trace` names cannot be
  read.
- A header is not the expected columns.
- A row repeats: the same `comment_id` and `target` in `comments.csv`, in two listed rows of
  `receipts.tsv`, or in two unlisted rows.
- The rows of `receipts.tsv` are not sorted. The report names the first row out of order.
- A pair of `comments.csv` has no listed row, or a listed row has no pair in `comments.csv`.
- A row of `receipts.tsv` names a comment that `comments.csv` does not hold, or an event that is
  not its comment's.
- A value is not one of its values: `listed`, `status`, `unknown_reason`, `evidence`, a time, an
  unlisted row's target, a mention mode or state. Or two values disagree: an `unknown` with no
  reason, a `received` with no evidence, a number of mention envelope ids that is not the number
  of mention modes.
- The rows of one comment in `comments.csv` disagree.
- A line of `receipts-counts.txt` is missing, extra, out of order or not in its form; a sum of
  its numbers is not true; a number in it is not the number that `report.ts` counts; or
  `rows_without_id` is not 0. The report names the line.

## Meaning search check

`meaning-search-check.ts` is a production check of Dispatch meaning search, and anyone can run it
again. A1 checks that a search answer was not degraded to keyword-only ranking.
A2 checks that a query sharing no word with its issue finds that issue in the top five. A3 reads
the coverage report: every item has a current vector from the expected model.

```bash
bun scripts/dispatch/meaning-search-check.ts \
  --a1-query 'meaning search' \
  --pair 'KEY=QUERY' \
  --lexeme-database "$TMPDIR/pg/.s.PGSQL.5432" \
  --coverage --expect-model '<the identity the deploy configures>' \
  --coverage-wait 600
```

Each check runs only when its flags are given, and at least one must be. `--coverage` and
`--expect-model` go together, and `--coverage-wait` (seconds, default 600) only with them.
`--pair KEY=QUERY` repeats, one per pre-registered pair, and splits at the first `=`; the operator
gives each pair's key at run time. Dispatch is resolved as for the census
above (`sessionDispatchSettings` over the envoy.json `command` reads, `DISPATCH_URL` and
`DISPATCH_TOKEN_FILE` or `DISPATCH_TOKEN` first). With Dispatch off it prints
`unavailable: config` and exits 2; it never prints the resolution's notices, which name files.

- **Build**, always: `GET /healthz`, sent no token, read on 200 and on 503. It prints
  `build commit=<sha|null> schema=<n|null> ok=<true|false>`. Whether that commit carries a given
  merge is checked outside the script, with
  `gh api repos/cybermonklabs/legion/compare/<merge>...<commit> --jq .status` (`ahead` or `identical`).
  An unreadable `/healthz` makes the run unavailable.
- **A1** (`--a1-query`): `GET /api/v1/search?q=<query>` fails when the answer has a `degraded`
  field, and passes when it has none and at least one result:
  `A1 degraded=yes|no results=<n> pass|fail`. An empty answer without `degraded` prints
  `A1 degraded=no results=0 unavailable: no-results`. A query with no indexable word gets an empty
  answer without the embedder being tried, so an empty answer proves nothing.
- **A2** (`--pair`): the issue's lexemes and the query's lexemes come from Postgres (below), and
  `shared` counts the query's distinct lexemes that the issue's row also holds. Then
  `GET /api/v1/search?q=<query>&limit=5`, with nothing else, gives `position`: where the issue's
  own row (kind `issue`, owned by the issue) stands among the first five results. A document,
  comment, ask or message the issue owns never counts. A pair passes when `shared=0` and the row
  is at position 1 to 5, and fails otherwise. Without `--lexeme-database`, or when the database
  fails, it prints `shared=unchecked` and is unavailable. When the issue read fails, it prints
  `A2 <KEY> shared=unchecked position=<1-5|none> unavailable: <category>`. Each pair prints
  `A2 <KEY> shared=<n|unchecked> position=<1-5|none> <verdict>`. When the pair's own search is
  unavailable it prints `A2 <KEY> unavailable: <category>`, with no shared and no position. A2
  passes when any pair passes, is unavailable when none passes and any is unavailable, and fails
  otherwise.
- **A3** (`--coverage`): `GET /api/v1/search/coverage`. A report that departs from its shape is
  malformed: `identity` a string or null, `place` `bedrock`, `azure` or null, at least one kind,
  each kind named `^[a-z][a-z0-9_]{0,31}$`, and `items = current + lacking + failed` for `total`
  and each kind. The check takes whatever kinds the report lists. It passes when `identity` is
  exactly `--expect-model`, `total.items` is above 0, and `total.failed` and `total.lacking` are 0
  in one snapshot. A fresh write is embedded a few seconds later, so while lacking items are all
  that is wrong it reads again every 5 seconds within `--coverage-wait`. When the wait runs out it
  fails on the last snapshot, whose `oldest_lacking_s` tells a write in flight from a stall. It
  prints `A3 identity=match|mismatch place=<place|none>`, the counts, `reads=<n>` and the verdict,
  then the counts of each kind.

The last line is `result pass|fail|unavailable exit=<code>`. Exit 0: every check asked for passed.
Exit 1: a check failed and none was unavailable. Exit 2: a check was unavailable, or a usage
error. An unavailable check
(`unavailable: auth|refused|transport|malformed|not-found|no-results|lexeme-database`) is never a
pass. `refused` is a 4xx refusal other than 401, 403, 404, 408 or 429, for example a query longer
than search accepts. A 5xx, 408 or 429 can clear on its own, so it is `transport`.

Output is allowlisted on stdout and stderr alike: keys, kind names, positions, counts, the build
commit, match or mismatch, the place and the failure category. The script never prints a URL, the
token, a response body, an issue title, a query, the coverage identity (on Azure it holds the
endpoint host) or an exception's text. This repository is public, and a search result's link
carries the private host while an issue title can carry a private name.

### Lexeme database

`--lexeme-database` takes a Postgres URL (`postgres://` or `postgresql://`) or the path of a
server's `.s.PGSQL.5432` socket. Any Postgres 18 works, as production and CI run 18: the check uses only
the built-in `english` configuration, in one statement per pair with every value bound, and
reads or writes no table. The statement mirrors the issue row's vector
(`migrations/0088_issues_title_lexemes.up.sql`, with `search_text` from
`migrations/0056_search_text.up.sql`) and search's own `websearch_to_tsquery('english', q)`. A
throwaway PostgreSQL 18.6, the version production runs, that listens only on a unix socket in
`$TMPDIR/pg`, from zonky's pinned Linux x86-64 binaries on Maven Central. The block is safe to run
again, also from a second shell at the same time: it holds a lock while it works, prints the socket
path and starts nothing when a server answers on the socket, refuses when none answers but
`$TMPDIR/pg/data/postmaster.pid` is left, downloads the server only when `$TMPDIR/pg/server` is
absent, creates the cluster only when `$TMPDIR/pg/data` is absent, then starts it and prints the
socket path:

```bash
pg="$TMPDIR/pg" v=18.6.0 && mkdir -p "$pg" && (
  flock 9 && cd "$pg" || exit 1
  # Ask the socket, never pg_ctl status: each role of an issue pod has its own PID namespace, so
  # postmaster.pid names a process only in the role that started the server.
  if python3 -c 'import socket, sys; s = socket.socket(socket.AF_UNIX); s.settimeout(5); s.connect(sys.argv[1])' "$pg/.s.PGSQL.5432" 2>/dev/null; then
    echo "$pg/.s.PGSQL.5432"
    exit 0
  fi
  if [ -e data/postmaster.pid ]; then
    echo "no server answers on $pg, but data/postmaster.pid is left: stop the server from the shell that started it, or remove $pg" >&2
    exit 1
  fi
  if [ ! -d server ]; then
    rm -rf server.new &&
      curl -fsSL -o pg.jar "https://repo1.maven.org/maven2/io/zonky/test/postgres/embedded-postgres-binaries-linux-amd64/$v/embedded-postgres-binaries-linux-amd64-$v.jar" &&
      python3 -c 'import zipfile; zipfile.ZipFile("pg.jar").extract("postgres-linux-x86_64.txz")' &&
      python3 -c 'import lzma, tarfile; tarfile.open(fileobj=lzma.open("postgres-linux-x86_64.txz")).extractall("server.new", filter="data")' &&
      mv server.new server ||
      exit 1
  fi
  if [ ! -d data ]; then
    rm -rf data.new &&
      server/bin/initdb -D data.new -U postgres --auth=trust -E UTF8 -c listen_addresses= -c "unix_socket_directories=$pg" &&
      mv data.new data ||
      exit 1
  fi
  # 9>&-: the server must not inherit the lock, or every later run waits on it.
  server/bin/pg_ctl -D data -l pg.log start 9>&- || exit 1
  echo "$pg/.s.PGSQL.5432"
) 9>"$pg/setup.lock"
```

If a run fails, run `rm -rf "$TMPDIR/pg"` and run the block again.

Pass it as `--lexeme-database "$TMPDIR/pg/.s.PGSQL.5432"`; the check connects as `postgres` to the
`postgres` database. Stop it with `server/bin/pg_ctl -D data stop` from `$TMPDIR/pg`, only in the
shell that started it: `pg_ctl` signals the PID in `postmaster.pid`, which in any other role of an
issue pod is another process or none. When no server answers and the block refuses, remove
`$TMPDIR/pg` and run the block again.

### The pre-registered pairs

The pairs, each a query and the key of the issue it should find, are recorded on the deployment's
issue; the keys are given only at run time. The queries:

- watching a running bot, the screen keeps scrolling down whenever output arrives, so earlier
  lines are impossible to study
- language model vendor billing balance ran dry and automated code checks halted
- flowchart markup inside pages shows as raw text instead of a drawn picture

Each query was fixed before the deploy and matched nothing on production then. A2 is judged on
the issue's own row because its key and title are the only text Dispatch indexes for that row
(`migrations/0088_issues_title_lexemes.up.sql`), and each query shares no lexeme with them. Each
query does share lexemes with its issue's spec, so a hit on a document the issue owns would be a
keyword match, not meaning search.

## Moved approval request check

`moved-approval-production-check.ts` is a production check of the report a document version
write carries when it takes an open approval request back from the person it was waiting on
(`moved_approval_request`), and anyone can run it again. It proves, on the running build, that
such a write names that request in its own answer: the ask the request stands on, the issue it
was opened in, and every document it names with the version it asks about beside the version
that document stands at now. It also proves the other half: a write of a document no open
request names carries no report at all.

```bash
bun scripts/dispatch/moved-approval-production-check.ts --project ACME
```

`--project` is required and takes one project key. **The check writes in production**, in the
project that key names: it makes a scratch issue of its own with a spec, uploads
`verification.md` and `notes.md` to it, requests approval of the spec with its Verification
document, writes the versions that move that request, and closes the scratch issue in a
`finally`, with a reason naming INFRA-3096, so a step that fails leaves no open approval request
in production. It touches no issue it did not itself create: an answer that is not the 201 of an
issue of the named project stops the run before a request is opened. Dispatch is resolved as for
the census above (`sessionDispatchSettings` over the envoy.json `command` reads, `DISPATCH_URL`
and `DISPATCH_TOKEN_FILE` or `DISPATCH_TOKEN` first). With Dispatch off it prints
`unavailable: config` and exits 2; it never prints the resolution's notices, which name files.

The steps print one line each, in this order:

- **Build**, always: `GET /healthz`, sent no token, read on 200 and on 503. It prints
  `build commit=<sha|null> schema=<n|null> ok=<true|false>`. This is no assertion of its own,
  since whether that commit carries a given merge is decided outside the script, with
  `gh api repos/cybermonklabs/legion/compare/<merge>...<commit> --jq .status` (`ahead` or
  `identical`). An unreadable `/healthz` makes the run unavailable.
- **Scratch**: the issue and its three documents, `scratch issue=<KEY>
  documents=spec.md,verification.md,notes.md pass`. Its status write must leave the issue
  `in_progress`: an issue that stands in any other status prints `status issue=<KEY>
  status=other fail` in place of that line.
- **Request**: `POST /api/v1/artifacts/{id}/approval-requests` with `include_verification`,
  `request ask=<uuid> include-verification=yes pass`.
- **Moved**, an assertion: `POST /api/v1/artifacts/{id}/edits` writes a new version of
  `verification.md`, and its own answer must carry the report. It prints
  `moved ask=<uuid|other> issue=<KEY|other> documents=<name>:<requested>/<version>,… pass|fail`:
  the ask id when it is the one the request opened, the issue key when it is the scratch issue's
  and no project stands beside it, and each document the report names with the version the
  request asks about and the version that document stands at now. It passes when all three hold
  and the entry for `verification.md` carries the version the request asks about beside the
  version this write stored. An answer with no report prints `moved report=none fail`.
- **Observed**: the same edit again, run through the pod's own `dispatch doc-edit` (`dispatch` on
  `PATH`, resolving Dispatch from this process's environment), `observed cli-line=yes|no
  exit=<status|none>`. This is an observation and no assertion, because a pod prints the line
  only once the `@sjawhar/pi-envoy` pin of the worker image carries this change. `exit=none` says
  the command could not be run at all.
- **Control**, an assertion: the same edit on `notes.md`, the document of that issue no open
  approval request names, whose answer must carry no report:
  `control document=notes.md report=none|present pass|fail`.
- **Close**, an assertion, in a `finally`: the closing reason as a message, then the close
  itself, which is sent whatever that message did, since the close is what withdraws the
  request: `close issue=<KEY> status=done|other pass|fail`. A reason that could not be posted
  is not reported, and costs the issue its closing words alone.

A step that could not be carried out prints `<step> unavailable: <category>`
(`auth|refused|transport|malformed|not-found|landed`) instead of its verdict and is never a pass;
the close, the one step that names a value before `unavailable:`, prints
`close issue=<KEY> unavailable: <category>`. A failed scratch or request stops the steps after
it, and the close still runs for an issue the run made.

A write that landed with no answer to give (`409 KEY_COMMITTED`, or a replayed 2xx reading
`ANSWER_NOT_KEPT`) made what it was sent to make, so the step reads it back and goes on from it,
printing `scratch|status|upload|request|close landed: read back` first: the scratch issue is the
newest open issue of the project holding the scratch title (`GET /api/v1/issues?project=`), an
upload the issue's document of that name (`GET /api/v1/issues/{key}/artifacts`), the request the
issue's open approval ask on the spec (`GET /api/v1/issues/{key}/asks?state=open`), and a status
write or the close the issue as it stands (`GET /api/v1/issues/{key}`), whose status is checked
as the write's own answer would be. Where the read finds nothing, the step prints `unavailable:
landed`. An edit that landed is `moved unavailable: landed` or `control unavailable: landed`: the
report it is judged by was in the answer Dispatch no longer has.

The last two lines are `result pass|fail|unavailable exit=<code>` and the count of the
assertions, `all 3 assertions passed` or `assertions passed <n> of 3`, so a reader can tell a
whole run from a cut one. Exit 0: every assertion passed. Exit 1: an assertion failed and no step
was unavailable. Exit 2: a step was unavailable, or the scratch issue stood in another status
(`status issue=<KEY> status=other fail`, no assertion, which stops the run as an unavailable
step does), or a usage error.

Output is allowlisted on stdout and stderr alike: the issue key the run made (checked against
the key pattern and the project the flag named), the names of the documents it uploaded, version
numbers, the ask id of the request it opened (checked as a uuid), a verdict word, a fixed failure
category and the build commit. Anything the server answers that is none of these is printed as
`other`, so a key, a name or an id of someone else's issue never appears. The script never prints
a URL, the token, a response body, an exception's text, an issue title, or the output of the
`dispatch` command it runs, of which only the exit status and whether the line appeared leave the
run. This repository is public, and such a line carries private hosts and names.

## Word rewrite

`word-rewrite/main.ts` is the Dispatch rewrite of INFRA-2017. INFRA-2017 removes one word from
this repository and from the text that agents wrote in Dispatch, and gives each use the words of
its own meaning. The tool finds the word in the titles and documents of open issues and in
project documents. It leaves each occurrence that the spec's last requirement (R6) allows, and
rewrites the others through Dispatch's own edit routes, which keep the old text in the
document's versions and the issue's events.

### The modes

The tool has three modes. Report mode reads and lists; it calls no model and writes nothing to
Dispatch. Write mode asks a model about each occurrence that no place leaves and writes each
rewrite. The settle command is write mode with the tester's answers in place of the model's, for
the occurrences whose two answers from the model failed.

```bash
bun scripts/dispatch/word-rewrite/main.ts report --tree INFRA-2017 --merge-commit <sha> \
  --state-document dispatch://INFRA/artifact/rewrite-state-md \
  --request-log "$TMPDIR/requests.log" --state "$TMPDIR/state.md"

bun scripts/dispatch/word-rewrite/main.ts write --tree INFRA-2017 --merge-commit <sha> \
  --state-document dispatch://INFRA/artifact/rewrite-state-md --extra-map "$TMPDIR/map.jsonl" \
  --summary 'INFRA-2017: <what the edit replaced>' --stop-after 90m

bun scripts/dispatch/word-rewrite/main.ts settle --merge-commit <sha> \
  --state-document dispatch://INFRA/artifact/rewrite-state-md --answers "$TMPDIR/answers.json" \
  --summary 'INFRA-2017: <what the edit replaced>'
```

`--help` prints every mode and flag. A flag the tool does not know, or a flag of another mode,
stops the run with `usage: <the reason>` and exit 2. Dispatch is resolved as for the census above
(`sessionDispatchSettings`, `DISPATCH_URL` and `DISPATCH_TOKEN_FILE` or `DISPATCH_TOKEN` first).

Report and write mode take these flags:

- `--tree <KEY>`: the root whose issues name the word as the subject of the change. The root
  and every issue under it are left (`this-tree`).
- `--merge-commit <sha>`: the merge that Dispatch's API names wait for. Before the merge it is
  the head, which no Dispatch build holds.
- `--project <KEY>` repeats. With none, the tool reads every project.
- `--include-closed`: read the closed issues too, and list their occurrences under
  `closed-issue`.
- `--state-document dispatch://<P>/artifact/<slug>`: the state kept between runs (see "The
  state"). Write mode needs it.
- `--state <file>`: a local copy of the state, written at the end of each run.
- `--extra-map <file>`, `--daemon-state <KEY>=<url>` (repeats) and `--no-daemon <KEY>`
  (repeats): see "The map".
- `--request-log <file>`: one line `METHOD path status` for each HTTP request, Dispatch's and
  each daemon's, never a host, a header or a body.
- `--stop-at <RFC 3339 time>` and `--stop-after <duration>`: see "The stop time".
- `--whole`: read every project again from the start, with the state's verdicts, rather than
  only the items that events name, the held-back writes and the items whose gate is reached.
  Report mode then writes no state, so it takes no `--state`.
- Write mode only: `--summary <text>` (required), `--model <id>` (default
  `anthropic/claude-opus-5-5`), `--concurrency <n>` (model calls at once, default 4),
  `--max-writes <n>`, and `--pause-before-write <file>`, which waits before each write until the
  file exists.

The settle command takes `--state-document`, `--answers <file>`, `--summary` and
`--merge-commit`, all four required, and `--daemon-state`, `--no-daemon`, `--extra-map` and
`--request-log` as above.

### Who may write

Write mode and the settle command run only with an agent credential that acts for no person
(`whoami` answers `kind: "agent"` with `owner: null`), and with a session to act as
(`DISPATCH_SESSION_ID`). A person's credential, and a personal agent token, record each write as
the person, which notifies the issue's route, claimant and authors. Each write acts as the
session that `DISPATCH_SESSION_ID` names, and the state keeps that session's id (see "Changed
since"). Without these, the run prints `refused: <why>` and exits 2.

### What it reads

In report mode every request is a GET. Every request of each mode waits its turn at five requests
a second. The tool reads `whoami`, `/healthz`, the projects, each project's open issues
(`GET /api/v1/issues?project=<K>&open=true`), each issue's documents, each project's own
documents, and each document's text with its blocks, whose ranges are UTF-8 byte offsets. It
reads the issues under `--tree`, and the events of an issue whose title holds the word. For a
document that has a version not made by sessions alone, it reads that version and the version
before it.

### The places a program can tell

`word-rewrite/places.ts` lists the reasons in the order they are checked. The first reason that
holds is the one counted:

- `credential-scope`: the credential's `project` is another project.
- `this-tree`: the issue is `--tree` or an issue under it.
- `closed-issue`: the issue is done.
- `approval-request`: an open approval request names the document.
- `approved-by-person` and `approved-earlier-open-ask` (`word-rewrite/approval-skips.ts`): a
  person approved the document at its latest version, or at an earlier version and the document
  holds an ask block not yet answered.
- `name-or-slug`: in the document's name or slug, or in the slug of a `dispatch://…/artifact/`
  link.
- `ask-block`: inside an ask block.
- `check-block`: inside a `check` block of the issue's Verification document, since a changed
  check voids the results recorded against it.
- `block-attribute`: in the opening line of any other typed block, its id or an attribute,
  since a changed id moves the block.
- `person`: a person's version made the block or changed its text.
- `unknown-author`: a version that made or changed the block has no author, a system author, or
  a person beside a session, so no program can tell that no person wrote it. A session is an
  agent whatever its owner, so an agent's write through a person's personal agent token is a
  session's text, which the tool changes.
- `code-name-not-yet`: a name of the map whose gate the project has not reached.

A document that a design gate names, with no approval, is changed like any other.

### The map

`word-rewrite/name-map.ts` holds every old name that this change renames and that Dispatch text
can hold, each with its new name and its gate. A name that the release of a project's daemon
carries (a file, a stored name, a Go identifier or test, a skill file) is rewritten only in a
project whose daemon reads at or above this checkout's contract, computed by
`bash scripts/daemon-contract.sh` at each run, or that the run names with `--no-daemon`. The
child operation that the daemon removed at contract 205 waits for
205. A name of Dispatch's API waits until the running Dispatch build holds `--merge-commit`. A
Go test's name that holds the word and that no row names is kept, since no program can tell its
new name.

For each project, the tool finds the daemon through `GET /api/v1/projects/<K>/legion/daemon`, or
through `--daemon-state <K>=<url>` where Dispatch names none, and reads `contract.daemon` from
`GET <url>/legion/v1/state` with no credential, within 60 seconds and 32 MiB. It prints
`daemon <K> source=<dispatch|run|none> contract=<n|no-contract-key|no-answer|unknown|no-daemon>`.
A daemon that does not answer, or a state with no contract, keeps every name. It prints the
Dispatch build as `dispatch commit=<sha|null> holds-merge=<yes|no>`: the build holds the merge
when `git merge-base --is-ancestor` says so in the tool's own checkout.

`--extra-map <file>` adds rows at run time, one JSON object per line, as the names of the other
repositories are not in this public repository:

```json
{"old": "OLD-NAME", "new": "NEW-NAME", "codeName": true, "from": "merged"}
{"old": "Old heading", "new": "New heading", "codeName": false, "from": {"contract": 230}}
```

`from` is `"merged"`, `{"contract": n}` or `{"dispatchHolds": "merge"}`. A line that is not such
an object stops the run with `--extra-map line <n>: <reason>` and exit 2.

### The judge and the writes

For each paragraph that holds occurrences that no place leaves and no verdict decided, write
mode makes one call to the deployment's model gateway (`word-rewrite/gateway.ts`). The prompt
gives the paragraph, the issue's status and its last five status moves, and the map's rows that
apply in that project. The answer is `{"rewrite": "<the paragraph>"}`, that with
`"left": {"<offset>": "<place>"}` for each marked occurrence it keeps, or
`{"allowed": "<place>"}`. A place is `quote`, `word-as-word`, `other-team-technical`,
`third-party-name` or `record`.

An answer that is not that shape, or a rewrite that changes text outside the sentences of the
occurrences, adds or drops a sentence among them, drops or changes an occurrence that it had to
keep, or still holds the word, fails the check.
The tool asks once more and says what failed. When the second answer fails too, the tool keeps
the occurrence as judge-refused (the code `j`), and no later run asks the model about it again.

A document gets one edit with every `replace` of the document, the token read as its
precondition and `--summary`. A title gets one issue update with `expected_sequence`. Just before
each write, the tool reads the item again, and writes nothing to an item that a place a program
can tell now holds whole: a closed issue, or a document that a person approved or that an open
approval request names. Dispatch's refusal is never forced: the write goes into the state's
held-back writes, and the next run reads the item again and tries again. A refusal with no edit
behind it (413 `CAP_EXCEEDED`, a 5xx) is tried once more at once.

When a write changes a paragraph, the tool saves under the new paragraph's hash the verdict of
each occurrence it left there, so its own edit leaves no occurrence unjudged.

Each item prints `write kind=<kind> project=<K> issue=<KEY|-> document=<slug|-> result=<result>`.

### The settle command

The settle command takes the tester's answers in place of the model's, and does what write mode
does with them: the same check, the same pace, the same read of the item just before its write,
the same verdicts saved, and every answer of one item in one write. It asks no model, reads only
its answers' items, and moves neither cursor of the state.

The answers file is one JSON object. Each answer has the model's shape and stands under the key of
a judge-refused occurrence of its paragraph, as a report lists it
(`judge-refused item=<id> key=<key> at=<offset>`). The offset is the occurrence's place in its
paragraph, which a rewrite's `left` names:

```json
{
  "<key>": {"rewrite": "<the new paragraph>", "left": {"<offset>": "quote"}},
  "<another key>": {"allowed": "quote"}
}
```

When an answer fails the check, the pace holds the write back, or Dispatch refuses it, the settle
command writes and saves nothing for that item. It prints why, and, for the pace,
`held: item=<id>: the pace lets it be written at <time>`. A settled item prints
`settled item=<id> result=<written|judged-left>`. An occurrence that a place a program can tell
holds needs no settling. The command exits 0 when it settled every answered item, and 1 when it
refused or held one.

### The pace

A named version and a title edit each give one card to each session subscribed to the owner's
topic, and a card starts a turn on an idle session. The tool cannot read who those sessions are,
so the pace bounds its writes:

- at most one write every 10 seconds, which the run waits out;
- at most one write for each owner (an issue, for its title and its documents, or a project
  document) in any 15 minutes;
- at most one write for each project in any minute.

The pace counts the writes of every run and settle, whichever process made them. The state holds
no time, so before each write the tool reads Dispatch's events: the title edits and named versions
of the sessions that its state lists. The event log answers only `after` and `limit`, so the tool
steps back from the newest event until it finds one older than 15 minutes, and reads forward from
there. It also reads each issue's own events. The tool's state saves are unnamed versions, which
the pace does not count.

A write that the owner's or the project's bound holds back waits for a later run, in the state's
held-back writes. Report mode prints `owner <owner> subscribers=unknown` for each owner whose
items wait for the model, then `pace writes=<n> run-time-s=<s>` for those items, and
`settle-pace writes=<n> run-time-s=<s>` for the settle command's writes, one for each item that
holds a judge-refused occurrence the report lists.

### The state

`--state-document` names a project document that holds one fenced `json` block. It holds:

- the cursor of the event log: the newest event id at the start of the first whole read, which
  each later run moves to the newest event it read before its first read of an item;
- the cursor of the last write-mode run (`write_cursor`);
- whether the first whole read has finished, and the projects it has finished;
- the verdicts on the occurrences that the model or the settle command left, each a one-letter
  code under a hash of the item id, the paragraph and the occurrence's place in it (`j` for an
  occurrence whose two answers failed);
- the held-back writes, by item id and keys, each marked when Dispatch refused it;
- `waiting`: each item that holds a name the tool kept, with its gate, a daemon contract (`c<n>`)
  or the merge (`m`);
- `refused`: each occurrence kept as judge-refused, by item id and key;
- `sessions`: the ids of the sessions that ran write mode or the settle command.

It holds only hashes, numbers, true or false, ids, project keys and one-letter codes, and no text
of any item. A state past 900 KiB is refused with `state-too-large`, which names the count of each
part.

Each mode reads the state at its start. Write mode saves it over the token it read, at most once a
minute while it holds something it has not saved, and once more when the run stops. Before its
first write, a run saves the state with its session's id, so a write whose later save fails is
still the tool's own edit. Each save is an upload with no summary, so Dispatch keeps it as an
unnamed version, which wakes no session. Report mode writes only its `--state` copy.

Until the first whole read has finished, a run reads the projects that it has not finished. After
that, a run reads the held-back writes, each item whose gate its project's daemon or Dispatch's
build has now reached, and each item that an event after the cursor names. An event of an issue
names its title and every document of it; an event of a project document names that document. A
run moves the cursor only to the newest event it read before its first read of an item, so an
edit made while it runs is read by the next run. A read that fails ends the run with
`unavailable: <route> <category>` and exit 2; the cursor stays, and the held-back writes that the
run did not reach stay in the state.

### Changed since

A report lists apart, as changed since, each occurrence not judged (a judge-refused one among
them) in an item that an event after the last write-mode run's cursor changed or took out of a
place R6 allows: a version of the document, its first included; the issue's creation; a title
change; a reopen (an `issue.updated` whose `changed` names `closed_at`); the `ask.answered` or
`ask.resolved` that closed an approval request naming the document; or the document's
`artifact.changes_requested`. A comment, a claim and a plain status move count for nothing. A
version or a title edit whose actor is a session that the state lists counts for nothing, so the
tool's own edits and the tester's settles are set aside by who made them, not by their summary.
Each line names the event, its author and, for a version, the version's link:
`changed-since <where> event=<id> author=<kind>:<id> version=dispatch://…@v<n>`.

### The stop time

`--stop-at` and `--stop-after` give a run a time to stop; with both, the earlier counts. At that
time the run starts no new read, judgement or write. It saves its state, prints its last lines
with `result stopped at its stop time <time>`, and exits 0. A project it was reading is read whole
again by the next run; an item that it read from events or held-back writes and did not handle
goes into the held-back writes.

### What it prints

One line for each occurrence:
`occurrence kind=<kind> project=<K> issue=<KEY|-> document=<slug|-> byte=<n|name> <place>`, where
the place is `reason=<reason>`, `verdict=<place>`, `judge-refused item=<id> key=<key> at=<offset>`,
`rewritten` or `not-judged`, with ` name=<old name>` where a map name covers the occurrence. A
judge-refused occurrence is listed under a place a program can tell while one holds it; the run
reads its item where it did not read it otherwise. An item that write mode writes is listed as
the run read it: each occurrence the write judged is `rewritten`, or under the verdict saved for
it under the new paragraph's key. Then the counts: `count <kind> <K>
occurrences=<n> not-judged=<n>`, `reason <reason> <n>` for every reason, `verdict <place> <n>`,
`rewritten <n>`, `judge-refused <n>`, `not-judged <n>` (judge-refused occurrences among them) and
`items-read <n>`. Then each check block of an open issue that names an old name of the map, the
built-in rows and the `--extra-map` rows alike,
`check-name issue=<KEY> document=<slug> block=<id> old=<name>`, with ` approved=latest` where a
person approved the block's document at its latest version (a change to the block would cost a
new approval), and `check-names <n> approved-latest=<m>`, where `<m>` counts the marked lines.
Report mode then prints the changed-since section with `changed-since <n>`, and the pace lines.

Last come `state verdicts=<n> held=<n> waiting=<n> not-judged=<n> bytes=<n> projected-bytes=<n>
bound=921600`, then `model-calls first=<n> second=<n> again=<n> judged-again=<n>` (first asks,
second asks after a failed first answer, first asks for a refused write tried again, and the
occurrences of those), `requests <n>`, `seconds <n>`, and `result done` or the stop line. A state
that cannot be written prints why and exits 1.

### The fixtures and the local proof

`word-rewrite/local-seed.ts --url <base> [--out <file>]` seeds a local Dispatch
(`bun run dispatch:local`) with the items that check k6 names, in the projects `SEED`, `BELOW`,
`ABOVE`, `NOKEY`, `NONE`, `NOADDR`, `GIVEN` and `SILENT`. It starts stub daemons below, at and
above the recorded contract and one whose state has no contract, registers them, and finds an
address that no daemon answers on, for `SILENT`. It prints one JSON object of every key, id and
address, with the flags of a run (`run_flags`), and serves the stubs until it is stopped. A stub
takes `PUT /contract` with `{"daemon": <n>}` and reports that contract from then on, as k6 raises
the daemon below the contract. Each document the tool writes to stands on an issue of its own
where k6 lets it, since the pace writes one item of an issue in 15 minutes. The tree's root stays
open: k6 closes it after the write mode's first run. Four paragraphs end with the sentence "The
checks ran on Tuesday.", which k6's proxy of the model gateway answers with a rewrite that the
check of each answer refuses. Before its last step the seed waits for the edit notice of alice's
last edit (an `artifact.human_edited` event, 30 seconds after the edit), so that
`event-log.json` holds that step's events alone. `--field-only` seeds what k6 seeds again on each
start of a stamped Dispatch build: a document that names a field of the priorities answer under
its old name, and a state document of `SEED` whose whole read is done and that keeps the document
waiting for the build to hold the merge. Its texts hold the word: they are the tool's test input.

`__fixtures__/word-rewrite/capture.sh` captures from that Dispatch each fixture the tests read,
with the request that answers it: `BASE=<base> bash capture.sh <the seed's --out file>`, with
`LEGION_DAEMON_URL` naming a real daemon and `DISPATCH_URL` the production Dispatch, each read
with one GET and no credential. Run it once on a freshly seeded Dispatch, since its writes change
what a second run would read. `inputs.json` holds the test texts that hold the word, so that no
test file but those of the pattern, the map and the judge holds it. `state-document.json` holds a
state of an earlier shape; its test checks that Dispatch serves it as it was uploaded and that
the tool refuses it rather than read it as empty.

Check k9's sample of changed items leaves out the versions of the state document.

The tests are `__tests__/word-rewrite-*.test.ts`, on fakes of Dispatch's reads and writes and of
the model (`__tests__/word-rewrite-fake.ts`). No test calls a model.
