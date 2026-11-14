# Measuring Dispatch search

This directory builds a fixture the size of a deployment's own Dispatch and measures what search finds in it.

It measures the keyword path alone: every search it sends carries `keywords_only=true`, the request the command bar sends behind a keystroke, so the route answers from `issues.search`, `document_search`, `comments.search`, `asks.search` and `messages.search` and makes no embedding call whatever embedder the server was configured with.
A server configured with none runs the same statement either way and stamps its answers `degraded`; one with an embedder answers the same rows without that stamp, because nothing is unavailable.
Nothing here writes an embedding, and nothing here needs a model service.

Every script reads the server it harvests from through its read API, with `GET` and no other method.

## The scripts

`dispatch_read.py` is the read-only client the others share: `DISPATCH_URL` and the token in the file `DISPATCH_TOKEN_FILE` names, a retry for a transport error or a 5xx, and a pool that runs several reads at once.

`pg.py` is the psql the others read the fixture with: a statement's rows as JSON, one object per line.

`corpus.py` is the corpus files the others write and read back — `projects.json`, `issues.jsonl` and the rest — and the progress line they say on stderr.

`harvest.py` writes a corpus into a directory under `$TMPDIR`.

- It reads every project, every issue of every project, every document of every project and every issue's primary document, each document's latest markdown, and every issue's comments, asks and messages.

- An issue's primary document is read from the issue's own artifact list, because `GET /api/v1/projects/{key}/artifacts` answers `and not is_primary` and leaves every spec out.

- `coverage.json` keeps the server's own item counts per kind, so a fixture can be compared with what it was harvested from; nothing else of that answer is kept, since its identity names a model service host.

`load_fixture.py` creates a database, migrates it and loads the corpus.

- The migration is this tree's own: `envoy-dispatch census` calls `store.Migrate` before it counts anything, the same call the server makes at boot, so the fixture's schema is the schema the server being measured expects.

- The rows go in with `COPY`, which fires every row-level trigger, and the triggers fill every `search` vector.

- Only what search reads and what the schema requires is written: a constant actor stands in for each row's author, and thread links, deliveries, events and block indexes are not written at all.

- An ask is loaded as a question, whatever kind it has in production, because `asks.approval` carries a schema-checked shape the read API does not answer whole, and neither `kind` nor `approval` is read by `asks_search` or by any leg of the search query.

`build_cases.py` turns a set of measured cases into descriptors.

- A case's query is Dispatch's own text and this repository is public, so a committed case names the item its text comes from, the version, the character range, and the SHA-256 of what that range yields.

- `case_text.py` holds the one rule for reading a range back, so the hash a descriptor carries is the hash of what the runner will send.

- A case whose query is nobody's text — a title someone reworded to ask search the same question in other words — cannot be described at all, and `build_cases.py` counts it and leaves it out.
That is kind 2 of the three kinds the baseline measured, and it is not measured here: a paraphrase is written text no document holds, so it names no item, has no character range and leaves nothing to re-extract and hash, while the paraphrase itself names an issue's subject, which this public repository may not carry.
The 121 kind-2 cases were committed as descriptors once and refused on every run, each with `no side file holds this query`; they are gone from `cases.json`, and what this measurement reports is kind 1 and kind 3.

`run_search.py` runs the cases against a search route and writes one JSONL row per case.

- Each query is read back out of the fixture the server is serving and checked against its descriptor's hash; a case whose text has moved is refused, never guessed at.

- A row carries ranks, counts and times, never a query and never a result's title, so the output of a run holds no Dispatch text.

- `--workers 1`, one request at a time, is the latency measurement; more workers measure recall faster and say nothing about latency.

`analyze.py` is both the table a person reads and the check a later run has to pass.

- It prints recall at 1, 3, 10 and 20, MRR and "absent from the page", per case kind and over the kinds together, each beside the baseline the hybrid route measured on the same cases, and the run's latency.

- It also counts, for a sentence from an issue's own spec, how often the issue's document comes back while the issue does not, which is the defect this measurement was made for.

- Then it asserts the floors, printing every rate with the n it was computed over, and exits non-zero naming each floor the run missed and by how much.
The floors are: kind 1 — R@1 at least 84%, R@3 at least 90%, R@20 at least 93%, absent from the top 50 at most 6%, and the document-without-the-issue defect exactly 0% at every depth; kind 3 — R@1 at least 79% and R@3 at least 83%.

- Each of those is set where the next harvest cannot cross it for a reason that is not the code, which is a lesson this tree paid for.
They were first read to a tenth of a point off one harvest of 4,369 issues; a harvest a day later, of 4,793, read kind 3's R@1 at 79.8% against a floor of 80.0%.
The same cases on that same fixture, served by the build of the day before, read 79.8% too, so what moved was the corpus rather than the search.
A floor a growing corpus steps over says nothing about a change, so each sits a whole point under the lower of the two harvests' readings, except where that reading is already a whole point clear.

- It then asserts the keyword path's latency on the `--serial` run: the server's own `took_ms`, median at most 100 ms, printed with the n it was read over, and a miss exits non-zero naming it and by how much.
The 95th and 99th percentiles are printed beside it and asserted against nothing.
That median is not the command bar's own latency, and reading it as the bar's median reads the wrong measurement.
A case here is a whole sentence of 8 to 183 words, median 77 — a line someone wrote in a spec, an ask or a comment — where the bar type-aheads on the three or four words a person has typed so far, and a search costs more the more lexemes it carries.
What the bar's keystroke costs is below this, and this measurement does not say by how much.

- 100 ms is a bound two pods and two harvests agree on.
On one pod, against the 4,369-issue harvest, ten serial runs read 29, 30, 30, 31, 31, 31, 43, 44, 45 and 45 ms; on a second pod, against the 4,793-issue one, the same cases read 55, 63 and 69 ms, and the build of the day before read 65 ms on that same fixture.
It is nobody's target, so every run it was read off passes it by construction, and what it catches is the run after a change — an embedding call back in the keystroke path, which costs about a second, or a leg that stopped using its index.
The same cases read 77, 79, 82, 98 and 449 ms at the median while the page cache was cold and `issues_search` carried a pending list, which 100 ms refuses.

- The tail is reported rather than asserted because it is not this path's to hold.
Two of the 104 cases of the serial sample match 16,454 and 20,578 rows and cost seconds on every run whatever the search does, and those same ten runs read a 95th percentile anywhere between 428 and 613 ms, against 416 and 480 ms for two runs of the build before this change on the same database and the same cases.
A ceiling over that spread would fail at random, which is worse than no ceiling.

- It was read against `issues.search` holding each issue's key, its title and its whole primary document, which migration `20261105145242_issues_search_spec` put there: 8.5 kB a row against 190 bytes before, and 36 MB of out-of-line values across the table.
A number read against a vector of a key and a title alone is not comparable with this one.

- It also assumes `issues_search` is carrying no backlog of pending entries.
A GIN index created with the default `fastupdate` batches new entries into a pending list that every scan of the index must read whole before it reads anything else; 1.8 MB of that list priced the index above a sequential scan of `issues`, and the issue arm then detoasted the whole table on every search (INFRA-1042).
Migration `20261108015800_issues_search_fastupdate` turns `fastupdate` off and empties the list, and `load_fixture.py` migrates a fixture before it copies a row into it, so the index a fixture writes into batches nothing.

- A run given no `--serial` pass measured no latency and says so, asserting nothing.

- The one thing to know about those floors: they are the numbers this tree measured, rounded to the whole point towards the bound, and nobody's target.
The run they were read off passes them by construction.
What makes them a check is the run after a change: a floor missed there is a regression, where without them the same regression prints as one more row of a table nobody diffed.

- The floors are aggregate, so a change that broadened every query could still clear them; `TestSearchBroadensOnlyTheQueryNoItemAnswers`, in `packages/envoy/internal/dispatch/api/`, pins that behaviour directly.

- A run that measured too few cases of a kind is void: `analyze.py` says so and exits non-zero instead of reading as a pass.
The minimum is nine tenths of the committed descriptors of that kind — 250 of the 278 kind-1 cases, 391 of the 435 kind-3 ones.
A run refuses a case whose text has moved since the descriptors were built, and a few such refusals are normal (the run of 2026-11-05 refused one); a run that measured a handful measured a broken fixture, a half-loaded corpus or a server that stopped answering, and a handful of cases may never read as a pass.

`serial120.json` is the 120 case ids of the latency sample, so a latency run is the same sample every time.

## A run, end to end

```bash
cd packages/envoy && go build -o "$TMPDIR/envoy-dispatch" ./cmd/dispatch
python3 scripts/search-measure/harvest.py --out "$TMPDIR/corpus" --workers 24
python3 scripts/search-measure/load_fixture.py --corpus "$TMPDIR/corpus" \
  --database searchfix --dsn "$LEGION_TEST_PG_DSN" --server "$TMPDIR/envoy-dispatch"
```

Then serve the fixture with no embedder.
With no AWS region reaching the process, `embed.NewBedrock` fails, the server logs `meaning search unavailable`, and `s.deps.Embedder` is nil, which is the branch `runKeywordOnlyDegraded` serves:

```bash
env -u AWS_REGION -u AWS_DEFAULT_REGION -u AWS_CONFIG_FILE -u AWS_PROFILE \
  DATABASE_URL="postgres://…/searchfix" DISPATCH_AGENT_TOKEN=fixture-token \
  DISPATCH_NATS_DISABLED=1 DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT=8799 \
  DISPATCH_SERVER_URL=http://127.0.0.1:8799 DISPATCH_SIGNING_KEY=fixture-key \
  "$TMPDIR/envoy-dispatch"
```

Build the cases once against the corpus, then run them:

```bash
python3 scripts/search-measure/build_cases.py --corpus "$TMPDIR/corpus" \
  --cases "$TMPDIR/cases_all.json" --out scripts/search-measure/cases.json
python3 scripts/search-measure/run_search.py --cases scripts/search-measure/cases.json \
  --fixture-dsn "postgres://…/searchfix" --url http://127.0.0.1:8799 --token fixture-token \
  --workers 8 --out "$TMPDIR/out-all.jsonl"
python3 scripts/search-measure/run_search.py --cases scripts/search-measure/cases.json \
  --fixture-dsn "postgres://…/searchfix" --url http://127.0.0.1:8799 --token fixture-token \
  --only scripts/search-measure/serial120.json \
  --workers 1 --out "$TMPDIR/out-serial.jsonl"
python3 scripts/search-measure/analyze.py "$TMPDIR/out-all.jsonl" --serial "$TMPDIR/out-serial.jsonl"
```

`cases.json` is the descriptor set as it was built against a harvest of 2026-11-05: 278 kind-1 descriptors and 435 kind-3 ones.
A case whose sentence its document no longer holds is refused by the runner and counted, which is how a later harvest says what has moved under it.
