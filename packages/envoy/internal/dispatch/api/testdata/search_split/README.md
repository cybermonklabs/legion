# Captured plans of search's split

Each JSON file here is one plan that PostgreSQL printed for `explain (analyze, buffers, verbose,
format json)` of one of search's statements. `TestTheSearchSplitFindsEveryArmOfACapturedPlan` and
`TestTheRankingIsTheKeywordArmsLessTheRankFreeArms` (`../../search_split_test.go`) read them, and
the split's parser was written from them (INFRA-4864's plan, D8). Each file is the psql output of
one run, as psql wrote it, with no change.

All eight files have these facts in common:

- Commit: `03eed5aedacc90732a610da00ee4dd4c669f4e83`, whose statements the files are the plans of.
  `search_split_test.go` was in the working copy on top of it and changed no statement.
- `select version()`: `PostgreSQL 18.7 (Debian 18.7-1.pgdg13+5) on x86_64-pc-linux-gnu, compiled by gcc (Debian 14.2.0-19) 14.2.0, 64-bit`,
  the pod's test Postgres (`$LEGION_TEST_PG_DSN`).
- Database: a database of its own on that server, seeded with
  `go -C packages/envoy run ./cmd/dispatch-seed --database-url "$URL" --scale 0.01`. At that
  scale the subject rules find a medium word (one in at least 1,000 items), so no capture at
  `--scale 0.1` was needed.
- Subjects (`subjects.json`): project `SEED1`, issue `SEED1-1`, model `seed-embed-v1`, words
  `brid` (rare), `geb` (medium) and `bab` (common). Every file is a plan of the medium word, `geb`.
- Each plan is run 1 of 5 of its statement on its side.

| File | Statement | Side | Form | Cut from |
| --- | --- | --- | --- | --- |
| `keyword.json` | `searchQuery` | head | one project | `split/plans/medium-project-keyword-head-1.json` |
| `keyword_rank_free.json` | `searchQuery` built with rank-free keyword legs | head | one project | `split/plans/medium-project-keyword-head-rank-free-1.json` |
| `meaning_every.json` | `searchQueryMeaning` | head | every project | `split/plans/medium-every-meaning-head-1.json` |
| `meaning_joined.json` | `searchQueryMeaningJoined` | head | one project | `split/plans/medium-project-meaning-head-1.json` |
| `meaning_scoped.json` | `searchQueryMeaningScoped` | head | one project | `split-scoped/plans/medium-project-meaning-head-1.json` |
| `main_meaning_every.json` | `searchQueryMeaning` as main ran it at df18d9be (the oracle's text then, since frozen again as `mainAt4975SearchQueryMeaning`) | `main` | every project | `split/plans/medium-every-meaning-main-1.json` |
| `main_meaning_joined.json` | `searchQueryMeaningJoined` as main ran it at df18d9be (since `mainAt4975SearchQueryMeaningJoined`) | `main` | one project | `split/plans/medium-project-meaning-main-1.json` |
| `main_meaning_scoped.json` | `searchQueryMeaningScoped` as main ran it at df18d9be (since `mainAt4975SearchQueryMeaningScoped`) | `main` | one project | `split-scoped/plans/medium-project-meaning-main-1.json` |

`main`'s three meaning plans hold the lateral read of `artifact_versions` in their document arm.

## The commands

`split` and `split-scoped` are two directories under `$TMPDIR`, and `$URL` is the seeded database.
The seed leaves the embeddings `project_key` fill not done (`embeddings_project_key_backfill.done`
is false), so the one-project form takes the joined meaning statement. The `split` capture is the
pipeline of `packages/envoy/scripts/README.md`, "The split of search's buffer reads", run from the
repository root:

```bash
OUT=$TMPDIR/split
DISPATCH_SEARCH_SPLIT_OUT=$OUT go -C packages/envoy test -count=1 -run '^TestSearchReadSplitScript$' ./internal/dispatch/api/
psql "$URL" -X -q -At -v ON_ERROR_STOP=1 -f "$OUT/subjects.sql" -o "$OUT/subjects.json"
DISPATCH_SEARCH_SPLIT_OUT=$OUT DISPATCH_BENCH_DATABASE_URL="$URL" DISPATCH_TEST_DATABASE_URL="$LEGION_TEST_PG_DSN" \
  go -C packages/envoy test -count=1 -run '^TestSearchReadSplitScript$' -v ./internal/dispatch/api/
psql "$URL" -X -q -v ON_ERROR_STOP=1 -f "$OUT/split.sql"
```

The `split-scoped` capture is the same after the fill is recorded as done. Every embeddings row of the
seed already holds its `project_key`, so that row then says what the fill would say. The
one-project form then takes the scoped meaning statement:

```bash
psql "$URL" -X -q -c "update embeddings_project_key_backfill set done = true"
OUT=$TMPDIR/split-scoped
DISPATCH_SEARCH_SPLIT_OUT=$OUT go -C packages/envoy test -count=1 -run '^TestSearchReadSplitScript$' ./internal/dispatch/api/
psql "$URL" -X -q -At -v ON_ERROR_STOP=1 -f "$OUT/subjects.sql" -o "$OUT/subjects.json"
DISPATCH_SEARCH_SPLIT_OUT=$OUT DISPATCH_BENCH_DATABASE_URL="$URL" DISPATCH_TEST_DATABASE_URL="$LEGION_TEST_PG_DSN" \
  go -C packages/envoy test -count=1 -run '^TestSearchReadSplitScript$' -v ./internal/dispatch/api/
psql "$URL" -X -q -v ON_ERROR_STOP=1 -f "$OUT/split.sql"
```

Each file was then copied from the plan the table names, for example
`cp "$TMPDIR/split/plans/medium-project-keyword-head-1.json" keyword.json`.

A plan of the scale-1 Job or of production whose node shape differs from these is added here as a
fixture of its own, with its row in the table, and the two tests read it.
