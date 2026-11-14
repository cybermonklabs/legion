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

- `check.sh` — the gate, in eight modes. `--help` lists its prerequisites and `--coverage` prints what it reads and what it does not.

- `check.test.sh` — the faults the gate was found passing, one case each: a row with a command and no predicate, an absence search whose corpus read failed, an absence search whose first of two reads failed, a landed page naming a commit of the base branch, a project citation whose quoted text the file does not hold, a committed copy whose bytes changed, a run that says nothing of a published copy's live address, a landed page naming a base commit that a forward merge brought in, a recorded command that runs a package with no version, a version written as `@latest`, a runner the reader did not read (`uv tool run`), a pinned runner with an option before its package, which must pass, a runner inside a string handed to a shell, a range written as a version, and forms no earlier case named, which the reader refuses because it fails closed. The tester found all of them but the eighth, found at this pull request's own forward merge of main, and the last, which the architect asked for.
  Cases 22 to 29 come from the review of the pull request: a reference to a Dispatch block at a version Dispatch does not hold, a package beside a constraints file, a failed stage before the last of the final pipeline, a failed read before `&&`, a bound over output that is not one number, a status other than the declared one, and an exit status beside a message the output does not carry. Each of cases 23 to 28 but the bound stands beside an honest form that passes. Case 29 plants a group in a pipeline stage, which is read as the last command it ran, beside its honest form. Cases 30 and 31 plant a program named by a path, or run through `command`, `builtin` or `exec`, which the evaluator cannot record and so refuses by name, beside a search named by its bare word that finds nothing. Cases 32 to 35 plant, for each admitted form, a failed read beside the honest form that passes, a program variable whose program is missing, one refusal per form the evaluator does not admit, and a predicate with a key of the wrong kind. Case 36 plants a row whose only program is a program variable, and case 37 four forms the admitted list does not name: arithmetic, in a command and in a word, and process substitution, in and out. Cases 38 and 39 plant $ forms the classifier does not list, $[ ] and ${o:-y}, which it refuses, beside a parameter written as ${o}, which it reads.
  Cases 41 and 42 break a scratch copy of `check.sh` and run its `--admitted` mode, which classifies every row of `claims.jsonl` through the classifier's one table of admitted forms and fails an entry no row uses, a row it refuses, and a header table that differs from the counts it measured: case 41 adds an entry no row uses, which `--admitted` fails by name, and case 42 takes out `&&`, an entry the rows use, so the rows that use it are refused. Case 43 plants `$_` and `${_}`, bash's special parameter for the last argument, which the classifier refuses, beside `$_x`, a name it reads. Cases 44 to 51 come from the fifth review: cases 44 to 49 plant a failed read hidden by a program `xargs` runs (`sh -c`, `bash -c`, `env`, `find -exec`), by an `xargs` option the table does not hold (`-I{}`, `--max-args`), by a shell reading `/dev/stdin` or `/proc/self/fd/0` as its script, by `export IOC_EXPECT_STATUS` or a loop over that name, by `unset -f`, an assignment, a redirection or `printf -v` that empties the record of failed reads, by an assignment to a program variable or to the shims' record, each beside an honest form that passes, and by `hash` and `head`, programs no row runs, and the gate refuses each by name; cases 50 and 51 break a scratch copy of `check.sh` so that a program variable leaves `PROGRAM_VARIABLES` or a name of the runner falls outside `RUNNER_PREFIXES`, and `--admitted` fails each by name. Case 52 plants a shell given a script path that a variable builds, `sh "$o"` with `o=/dev/stdin`, which is refused, because a script path is a literal file or one under `$IOC_EVID`, beside such a script under `$IOC_EVID`, which passes. Cases 53 to 57 come from the sixth review and plant a failed read hidden by a `printf` format that expands to `-v`, from a variable or a substitution, by `env -S`, by `uv run` running `sh -c`, with an option or a program its table does not hold, by a carriage return or a no-break space that bash keeps inside a word, and by an assignment to `PATH`, each refused by name, beside a literal `printf` format, `env` with no option and `uv run` running `python`, which pass. Cases 58 to 60 come from the seventh review and plant an unpinned `--with` after `uv run --python 3.12`, which `install-pinned` now reads, beside the wheel `env.sh` builds, which passes; an assignment or a for loop to a name the rows do not assign, `FUNCNEST` alone, in a subshell and as a loop's variable, `BASH_ENV`, `IFS`, `LC_ALL` and `PATH`, each refused by name, beside names the rows assign, which pass; and `IOC_EXPECT_STATUS` before another prefix whose substitution fails, which is refused, beside the declared status as the last prefix, which passes.
  Case 61 plants a `uv pip install` of a local source path with no build constraint and a `uv run --with` of a local source path, which `install-pinned` refuses because a build backend is an install, beside the same install with `-b` and the same run with a built wheel, which pass. Case 62 plants a pinned registry install that reads no lock and one whose requirements file under `evidence/` carries no hash, which `install-pinned` refuses as installs that read no lock, beside the same install reading `evidence/ioc-venv.lock`, which passes. The rule `install-pinned` holds, and what it cannot see, are stated once, in `check.sh`'s header above the reader (`reader_py`). The evaluator fails closed: it admits only the command forms the rows use, refuses every other by name, and holds an admitted row to its failed-read rule; the admitted forms, that rule and what it cannot see are stated once, in `check.sh`'s header above `run_recorded`. `--self-test` runs these cases, so one run proves the gate's own table and these.

- `sources.json` — every repository and commit, the instant every package index read is held to with the lock of the python environment, every published page with its committed copy, and each landed step with its proof and the sentences `--published` reads on the default branch.

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
bash docs/research/internet-of-cognition/check.sh --admitted
bash docs/research/internet-of-cognition/check.sh --published
bash docs/research/internet-of-cognition/check.sh --coverage
```

It needs `git`, `curl`, `jq`, `python3`, `uv`, `go`, `gh` and the `dispatch` command, and the Dispatch token this pod already holds; it clones what it needs under `$TMPDIR/ioc-clones`, one commit deep and with no tags, which is the clone the triage read.
`--replay` sources `env.sh` first, which builds under one cache directory everything a recorded command reads that this tree cannot hold: the two records under check, a python environment, the pinned model, mas-lab installed from its clone, the wheel of ioc-concord, the SHACL shapes of the scope clone and the review corpus. `env.sh`'s own header names each variable it exports, and it refuses with a line naming what it could not build. Every package the replay installs comes from a committed, hashed file, resolved as the package index stood at 2026-11-12T07:55:56Z, the instant `sources.json` records (the commit that recorded the model rows' outputs): `env.sh` syncs the python environment exactly to `evidence/ioc-venv.lock`, the 71 packages of the four requirements in `evidence/ioc-venv.in`, pyshacl among them, so the scope rows run pyshacl on the same rdflib as the shapes generator; the mas-lab installs read `evidence/tm-constraints.txt`, its 24 third-party packages hashed; and uv checks every hash. A build backend is held to the rule an install is: every local package the replay builds declares `hatchling` with no version, so every `uv pip install` and `uv build` of a local source, in `env.sh`, in the evidence scripts and in the rows, names `evidence/build-constraints.txt`, hashed too, with `-b`, and the tc-run rows run the ioc-concord wheel `env.sh` builds once under it, since `uv run` takes no build constraint. `install-pinned` refuses an install from an index that reads no hashed lock under `evidence/`, a local source install with no `-b` and a `uv run --with` of a source path.
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
