# The Internet of Cognition stack, read against Legion, Envoy and Dispatch, November 2026

This directory is the evidence under dispatch://INFRA-3134.
The judgement itself is a Dispatch document of that issue, dispatch://INFRA-3134/artifact/judgement-md: a front page with the ranking and seven candidate pages, each carrying its verdict, its cost, its owner, its default and the test that could have killed it.

The organisation read is `github.com/ioc-project`, which holds five repositories.
Three hold code: `mas-lab`, `scope` and `cognition-protocols`, the last of which holds CONCORD, a written specification with a reference library, and ACCORD, two agent skills with a merge script.
Each was read at one commit, which `sources.json` pins.

## The answer, in one screen

Nothing is taken wholesale, and no verdict on the list is `vendor` or `depend`.
Two steps are landed in the pull request that carries this directory, both in the skills every Legion reviewer and implementer reads: each review round's shortfall is recorded as two grader rows rather than one verdict, and the rule that combines the two graders is written where the reviewer reads it.
Three candidates stand above them in cost and below them in order: one definitions record, an execution record under the OpenTelemetry `gen_ai.` names, and one optional field on the Envoy envelope carrying a node reference.
One candidate is a page rather than a step, because it would refuse completions in every other tree.
One is rejected: MAS Lab's experiment harness, because the rig it would replace already exists here.

## What is here

- `questions.md` — the question list, written from our own code and issues before any deep read. Its commit is an ancestor of every commit that adds a deep-read evidence file, which `--shape` reads from the history.

- `claims.jsonl` — one row per claim: the form of evidence it was made on, the command that was run, the predicate over that command's output, and the mark a second reader gave it.

- `citations.py` — the one rule that turns a record into citation rows. `--self-test` proves the three readings it settles.

- `env.sh` — what `--replay` sources before its first recorded command: it prepares every input the tree cannot hold and exports one variable per input.

- `check.sh` — the gate, in seven modes. `--help` lists its prerequisites and `--coverage` prints what it reads and what it does not.

- `check.test.sh` — the faults the gate was found passing, one case each: a row with a command and no predicate, an absence search whose corpus read failed, an absence search whose first of two reads failed, a landed page naming a commit of the base branch, a project citation whose quoted text the file does not hold, a committed copy whose bytes changed, a run that says nothing of a published copy's live address, a landed page naming a base commit that a forward merge brought in, a recorded command that runs a package with no version, a version written as `@latest`, a runner the reader did not read (`uv tool run`), a pinned runner with an option before its package, which must pass, a runner inside a string handed to a shell, a range written as a version, and forms no earlier case named, which the reader refuses because it fails closed. The tester found all of them but the eighth, found at this pull request's own forward merge of main, and the last, which the architect asked for.
  Cases 22 to 29 come from the review of the pull request: a reference to a Dispatch block at a version Dispatch does not hold, a package beside a constraints file, a failed stage before the last of the final pipeline, a failed read before `&&`, a bound over output that is not one number, a status other than the declared one, and an exit status beside a message the output does not carry. Each of cases 23 to 28 but the bound stands beside an honest form that passes, and a group in a pipeline stage, read as its last command, beside its honest form.
  The rule `install-pinned` holds, and what it cannot see, are stated once, in `check.sh`'s header above the reader (`reader_py`). The failed-read rule the evaluator holds every recorded command to is stated once, in `check.sh`'s header above `run_recorded`. `--self-test` runs these cases, so one run proves the gate's own table and these.

- `sources.json` — every repository and commit, every published page with its committed copy, and each landed step with its proof and the sentences `--published` reads on the default branch.

- `triage/` — one file per repository of the organisation, written before any deep read: what it is, its maturity read from its own releases and history, and which of its parts could cross into our runtime at all.

- `mas-lab/` — one file per directory of the component nobody had opened, and the three questions this issue asks of it.

- `interview/` — the authors' own account of 2026-11-11, committed whole, and one row per substantive claim it makes.

- `similarity/` — ACCORD's own term comparison, re-run on both its backends over nine definition pairs, four of them ours.

- `coordination/` — our own three alignment problems, counted over a fixed window of our own record, with the walker that counted them.

- `tests/` — the three tests against our own records: a real grader disagreement through the shipped CONCORD library, a real Dispatch issue as Turtle against SCOPE's ontology, and a real Legion phase workflow as a MAS Lab specification.

- `node-model.md` — the node model read back, with the four gaps this reading found in it.

- `rejected.md` — the candidates that are on no page, each with the reason.

- `evidence/` — the recorded output of each run, as the run wrote it.

- `published/` — a committed copy of every published page a claim quotes, because a page served from a content system changes its bytes between any two fetches, and under `github-api/` the answers GitHub's REST API gave for the five repositories of the organisation.

## Running the gate

```
bash docs/research/internet-of-cognition/check.sh --replay
bash docs/research/internet-of-cognition/check.sh --shape
bash docs/research/internet-of-cognition/check.sh --citations
bash docs/research/internet-of-cognition/check.sh --landed
bash docs/research/internet-of-cognition/check.sh --self-test
bash docs/research/internet-of-cognition/check.sh --published
bash docs/research/internet-of-cognition/check.sh --coverage
```

It needs `git`, `curl`, `jq`, `python3`, `uv`, `go`, `gh` and the `dispatch` command, and the Dispatch token this pod already holds; it clones what it needs under `$TMPDIR/ioc-clones`, one commit deep and with no tags, which is the clone the triage read.
`--replay` sources `env.sh` first, which builds under one cache directory everything a recorded command reads that this tree cannot hold: the two records under check, a python environment, the pinned model, mas-lab installed from its clone, the SHACL shapes of the scope clone and the review corpus. `env.sh`'s own header names each variable it exports, and it refuses with a line naming what it could not build.
Each mode but `--coverage` prints one `PASS` or `FAIL` line per assertion and a last line reading `all N assertions passed`; `--coverage` prints the assertion table and the list below it.
Read a run by that line and not by its exit status alone: a run that was killed prints nothing and exits non-zero for a reason that is not this deliverable's.
`--shape` and `--landed` read the commits of the pull request that carried this directory, and main holds only its squash, so a reader of main fetches that history first and names it, as `git fetch origin refs/pull/3771/head:refs/remotes/origin/pr-3771` and then `IOC_HISTORY_REF=origin/pr-3771`; with no `IOC_HISTORY_REF` the two modes read `HEAD`, which is right on the pull request's own branch.

The answers GitHub's REST API gave on 2026-11-12 are committed under `published/github-api/`, and the rows of the triage read those copies.
An unauthenticated address is allowed sixty requests an hour, and a gate that read that API again would fail on GitHub's refusal rather than on the deliverable, which is what a run of 2026-11-12 did.
`--replay` still reads the budget first and refuses, naming it and the moment it resets, for the few rows that read a live address.

Nothing in CI runs this script.
It is run by hand, in a Legion pod, at the head that merges, and each check of dispatch://INFRA-3134 records its own result.

## What the gate does not read

`--coverage` prints this list beside the assertion table, and that printing is the one this directory keeps:
whether the ranking is right, whether the reconciliation is fair, whether the reading is deep, whether a cited text supports the claim beside it, whether a candidate that cannot be argued in one page was left off the list, whether one fact is written once, and whether an engineer-day estimate is right.
The tester and the reviewer read those by reading the deliverable against the question list.
