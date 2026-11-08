# Workflow engines, measured against Legion's daemon, November 2026

This directory is dated evidence that no job builds again.
Nothing in CI compiles, vets or runs the Go module under `prototype/`, and nothing re-records the output files.
They are the transcript of one run, on the date and at the commit their own headers name.

The answer this evidence was gathered for is the Dispatch document dispatch://INFRA-1546/artifact/temporal-evaluation-md, on the issue dispatch://INFRA-1546.
The reasoning lives there and is not copied here, so the two cannot disagree.

## The recommendation, in one screen

**Adopt named Temporal patterns inside Legion and run no engine.**

Legion's unit of work is a prompt into a session that already holds state, and Temporal's ordinary recovery is retry.
But retry is not Temporal's only shape: experiments 19 and 20 measured Asynchronous Activity Completion, where an activity's body returns `activity.ErrResultPending` and the work is handed out rather than run.
The server then kept that activity `Started` on attempt 1 with the process holding it killed, dispatched it to no replacement worker, took its outcome from a process that ran no worker and held only the task token, refused a token whose attempt had moved, and across five minutes with nobody holding it nothing but its own start-to-close timeout touched it.
So the state this investigation first said no measured product had is Temporal's, and the recommendation does not rest on its absence.
Experiment 21 then answered what this investigation called its own open question: the handout reaches a process holding what a pod holds and nothing of the engine, a working copy of its own and a placeholder credential, both with the token carried to it out of band and on a queue it named for itself, and the commit it made in its own repository arrived at the workflow.
For the out-of-band route the engine was told nothing whatever about that process, which is why the price of moving parts of the daemon into a product is re-derived lower here, 45 to 95 engineer-days rather than 60 to 120.
So the recommendation rests on a comparison of two figures derived at this head rather than on a mechanism nobody has: 37 to 53 engineer-days with nothing new to run, against 45 to 95 unverified plus a server to operate or a vendor to pay and a cut-over draining 81 trees in flight.
Its one mechanism for work pinned to a process is still the worker session, and experiment 3, re-measured with the workflow kept running and a replacement worker polling, shows a session does not recover: the body ran once and never again, the server held the killed attempt `Started` and then ended it `ActivityTaskTimedOut`, and the work sat on a `<resourceID>@<hostname>` queue no poller touched after the kill.
Temporal's own published coding-agent experiment reaches the same place and builds the recovery in the application: a claim on disk before each tool call, a lease on the transcript, a write guard against a stale worker's append.
Legion has those three already, written for the same reason.

At most 51.6% of `packages/daemon` is the kind of code a workflow engine is for, and measured by file inside its two largest mixed packages the figure is 40.7%.
The parts an adoption would delete are the phase table and its engine, 1,294 lines, and the outbox's three files, 2,751 lines.

At the measured rate Temporal Cloud costs about $3.37 a month in Actions, so money decides nothing here.

The ideas worth taking are the effect sandwich, a terminal "stopped, a person must look" state, one jittered backoff with a rate over a window, three timeouts rather than one, and a completion any process may report fenced by the attempt it was handed out at.

The directory keeps the name it was created with, because the issue's spec pins that path.
It holds the measurement of five products, not one.

## The twenty-one experiments

Five products were measured in one Legion worker pod, each against a real server or a real PostgreSQL.
Each experiment shows that a mechanism exists, not that it holds at Legion's scale, and each states that limit in its own output.

**Temporal**, nine experiments, each in a namespace of its own against one dev server the runner starts.

- `experiment-1-phase-table.txt` — Legion's lifecycle from `admitted` to `done` as one workflow driven by signals, with a backward move from reviewing to implementing and a second entry into testing.

- `experiment-2-human-gate.txt` — the design gate as a wait for a person's approval, withdrawn by a later document version and reopened by a second approval, with the cost of a wait of days split into what this run timed and what it reasoned from the mechanism.

- `experiment-3-session-activity.txt` — a session-bound activity whose worker process is killed while it runs, with the workflow held open by a signal so a replacement worker polls the queue while the run is still going, and one line per execution of the activity's body so a reader can count them.
  It has its own control: a run that killed no worker fails, and `run.sh` runs that control for each of the five kill experiments that have one, 3, 7, 10, 13 and 16.

- `experiment-4-delivery-once.txt` — a write to an outside system under an idempotency key with a crash after the write, and a delivery that can never succeed under a bounded retry policy.

- `experiment-5-admission.txt` — a cap of four across many workflows as a long-lived gatekeeper workflow, with its own negative control: the same workflows with the cap removed.

- `experiment-6-change-and-replay.txt` — an ordinary change to the workflow replayed against a recorded history, then the same change behind `workflow.GetVersion`, and the run's Actions counted two ways.

- `experiment-19-temporal-handout.txt` — an activity handed out with `activity.ErrResultPending`, its worker killed, a replacement worker polling the same queue, a completion from a process that holds only the task token, the refusal of a superseded attempt's token, and what the start-to-close timeout does when nobody completes it.

- `experiment-20-temporal-handout-held.txt` — the same handout with a ten-minute start-to-close and no heartbeat, held open for five minutes of real time with no worker alive, polled every thirty seconds, then completed by token: the reading that says nothing but that bound reaps a handout.

- `experiment-21-temporal-handout-to-a-holder.txt` — the same handout reaching a process that holds what a Legion pod holds and nothing of the engine, a working copy of its own and a placeholder credential, twice: once with the token carried to it out of band and no Temporal worker in it at all, and once on a task queue it named for itself, each ending with the commit it made in its own repository arriving at the workflow.

**DBOS**, a library in the process with its state in PostgreSQL.

- `experiment-7-dbos-kill.txt` — a step whose process is killed inside it, with its own control.

- `experiment-8-dbos-human-gate.txt` — a wait for a person, and what it holds.

- `experiment-9-dbos-change.txt` — a change against a recorded run, the version gate, and the patch.

**Restate**, a separate server the runner installs at a pinned version.

- `experiment-10-restate-kill.txt` — a handler whose process is killed inside it, with its own control.

- `experiment-11-restate-gate-and-pause.txt` — an awakeable held for a person, and the paused state an operator resumes.

- `experiment-12-restate-change.txt` — a change against a recorded journal, and the deployment versioning that makes it safe.

**go-workflows**, a library in the process with its state in PostgreSQL.

- `experiment-13-goworkflows-kill.txt` — an activity whose process is killed inside it, with its own control.

- `experiment-14-goworkflows-human-gate.txt` — a wait for a signal, and what it holds.

- `experiment-15-goworkflows-change.txt` — a change against a recorded run, which has no version guard.

**`durabletask-go`**, a library in the process with its state in PostgreSQL.

- `experiment-16-durabletask-kill.txt` — an activity whose process is killed inside it, with its own control.

- `experiment-17-durabletask-human-gate.txt` — a wait for an external event, and what it holds.

- `experiment-18-durabletask-change.txt` — a change against a recorded run, its patch guard and its versioned registry.

## Running it again

```sh
bash docs/research/temporal-evaluation-2026-11/run.sh
```

It installs the pinned Temporal command and the pinned Restate server under `$TMPDIR`, verifying each one's checksum and version, runs `gofmt -l` and `go vet` over the prototype, builds its six commands, runs the twenty-one experiments in order, runs the negative control of each of the five kill experiments that have one and requires it to fail, and ends with `temporal-evaluation: all 21 experiments passed`.
It writes nothing into the working tree.
Set `TMPDIR` to a directory on a volume with about 300 MB free; the script refuses an unset one and one under `/tmp`.
Set `LEGION_TEST_PG_DSN` to a PostgreSQL the run may create a database in; nine of the twenty-one need it (7 to 9, and 13 to 18), and the script refuses an empty one by name.
Run one at a time: two runs share one directory under `$TMPDIR` and collide.

To build or test the Go program by hand, from this directory:

```sh
cd prototype && GOWORK=off go vet ./...
```

`GOWORK=off` is required, because the repository's root `go.work` binds only `packages/daemon` and `packages/envoy` and this module is in no workspace.

## Checking the answer's citations

```sh
bash docs/research/temporal-evaluation-2026-11/check-citations.sh
```

It reads the answer and the plan live from Dispatch and resolves the citations of both that its grammar reads: a Legion file and identifier in either order, a quoted literal, a table or a column with its table, a numbered experiment and the figures attributed to it, a `dispatch://` reference, and a web address with the date it was read.
A figure resolves only against the measured lines of its experiment's transcript, below the header's dashed rule, as a number of its own, so a figure that stands only in a header, or only inside a longer number, fails.
An experiment this directory holds no transcript for is refused, its number standing above the highest transcript here or in a gap below it alike, and its figures are refused with it, so the count can never read as a pass for a figure nothing looked at.
It does not read every citation those documents carry, and the gaps are these four.
A dotted identifier with no path beside it is read by no rule.
A span mixing a code citation with another kind is read only in the part that matches.
A figure of one digit is not read at all, which includes the counts the comparison at the heart of the answer rests on, two side effects against one: those were checked by hand, by the tester and again by the reviewer, rather than by any rule here.
And a scope never crosses a table cell, so a figure in a table is not attributed to the experiment the neighbouring cell names.
What the count covers is that grammar and nothing wider, and whether a claim is missing its citation altogether is the reviewer's reading, as the issue's Verification document says in its own words.
It ends with `citations: N resolved, M unreachable, 0 failed`.

```sh
bash docs/research/temporal-evaluation-2026-11/check-citations.sh --self-test
```

runs the resolver over broken citations held in the script, so a reader can see it refuse what it claims to refuse without editing the live document.

## What is here

- `run.sh` — the runner.

- `check-citations.sh` — the citation checker and its self-test.

- `experiment-*.txt` — the recorded output of the run the answer quotes, one file per experiment, each with its date and commit in the header.

- `prototype/` — the Go program, holding only `*.go`, `go.mod` and `go.sum`, on the Temporal Go SDK v1.50.0, DBOS Transact v1.8.0, the Restate Go SDK v1.2.0, go-workflows v1.4.2 and `durabletask-go` v0.16.0.
