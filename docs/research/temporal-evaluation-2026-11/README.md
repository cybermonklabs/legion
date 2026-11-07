# Temporal, measured against Legion's daemon, November 2026

This directory is dated evidence that no job builds again.
Nothing in CI compiles, vets or runs the Go module under `prototype/`, and nothing re-records the output files.
They are the transcript of one run, on the date and at the commit their own headers name.

The answer this evidence was gathered for is the Dispatch document dispatch://INFRA-1546/artifact/temporal-evaluation-md, on the issue dispatch://INFRA-1546.
The reasoning lives there and is not copied here, so the two cannot disagree.

## The recommendation, in one screen

**Adopt named Temporal patterns inside Legion and run no engine.**

Legion's unit of work is a prompt into a session that already holds state, and Temporal's recovery is retry.
Its one mechanism for work pinned to a process is the worker session, and experiment 3 shows that a session does not recover: the process holding a session-bound activity was killed, the workflow saw a cancellation, the session's next activity saw `session has failed`, and nothing re-ran the work on any worker.
A session's activities run on a task queue named for the worker process, so a replacement worker never polls it.
Temporal's own published coding-agent experiment reaches the same place and builds the recovery in the application: a claim on disk before each tool call, a lease on the transcript, a write guard against a stale worker's append.
Legion has those three already, written for the same reason.

At most 51.6% of `packages/daemon` is the kind of code a workflow engine is for, and measured by file inside its two largest mixed packages the figure is 40.7%.
The parts a Temporal adoption would delete are the phase table and its engine, 1,294 lines, and the outbox's three files, 2,751 lines.

At the measured rate Temporal Cloud costs about $3.37 a month in Actions, so money decides nothing here.

The four patterns worth taking are a bounded retry policy whose end is a routable error, one retry policy where the work is declared rather than three backoff implementations, a heartbeat with a deadline on the unit of work, and the explicit withdrawal of an approval when a new document version lands.

## The six experiments

Each experiment ran in a Temporal namespace of its own, `e1` through `e6`, against one dev server started by the runner.
Each shows that a mechanism exists, not that it holds at Legion's scale, and each states that limit in its own output.

- `experiment-1-phase-table.txt` — Legion's lifecycle from `admitted` to `done` as one workflow driven by signals, with a backward move from reviewing to implementing and a second entry into testing.

- `experiment-2-human-gate.txt` — the design gate as a wait for a person's approval, withdrawn by a later document version and reopened by a second approval, with the cost of a wait of days split into what this run timed and what it reasoned from the mechanism.

- `experiment-3-session-activity.txt` — a session-bound activity whose worker process is killed while it runs.
  This is the run's control: a run that killed no worker fails.

- `experiment-4-delivery-once.txt` — a write to an outside system under an idempotency key with a crash after the write, and a delivery that can never succeed under a bounded retry policy.

- `experiment-5-admission.txt` — a cap of four across many workflows as a long-lived gatekeeper workflow, with its own negative control: the same workflows with the cap removed.

- `experiment-6-change-and-replay.txt` — an ordinary change to the workflow replayed against a recorded history, then the same change behind `workflow.GetVersion`, and the run's Actions counted two ways.

## Running it again

```sh
bash docs/research/temporal-evaluation-2026-11/run.sh
```

It installs the pinned Temporal command under `$TMPDIR`, starts a dev server there on a free port, runs `gofmt -l` and `go vet` over the prototype, runs the six experiments in order and ends with `temporal-evaluation: all 6 experiments passed`.
It writes nothing into the working tree.
Set `TMPDIR` to a directory on a volume with about 200 MB free; the script refuses an unset one and one under `/tmp`.

To build or test the Go program by hand, from this directory:

```sh
cd prototype && GOWORK=off go vet ./...
```

`GOWORK=off` is required, because the repository's root `go.work` binds only `packages/daemon` and `packages/envoy` and this module is in no workspace.

## Checking the answer's citations

```sh
bash docs/research/temporal-evaluation-2026-11/check-citations.sh
```

It reads the answer live from Dispatch and resolves every citation in it: a Legion file and identifier, a quoted literal, a table or a column with its table, a numbered experiment, a `dispatch://` reference, and a web address with the date it was read.
It ends with `citations: N resolved, M unreachable, 0 failed`.

```sh
bash docs/research/temporal-evaluation-2026-11/check-citations.sh --self-test
```

runs the resolver over broken citations held in the script, so a reader can see it refuse what it claims to refuse without editing the live document.

## What is here

- `run.sh` — the runner.

- `check-citations.sh` — the citation checker and its self-test.

- `experiment-*.txt` — the recorded output of the run the answer quotes, one file per experiment, each with its date and commit in the header.

- `prototype/` — the Go program, holding only `*.go`, `go.mod` and `go.sum`, on the Temporal Go SDK at v1.50.0.
