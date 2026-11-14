---
title: Task delivery cost
description: Read from the daemon's log what task delivery cost in one window of time, with the legion task-cost command, and what each figure means.
sidebar:
  order: 7
---

This page is for the operator.
The Legion daemon gives each worker its task as a numbered value.
The worker takes the task when its session is idle.
The daemon writes a line to its log for each step of that delivery.
The `legion task-cost` command reads these lines for one window of time.
It prints the cost of the delivery in that window, and three verdicts.

## Read the cost

Run this command where `kubectl` can read the logs of the `legion` namespace:

```sh
kubectl -n legion logs deploy/legion-<project>-daemon -c legion --since-time <from> | legion task-cost --from <from> --to <to> [--claim <token>]
```

- `legion-<project>-daemon` is the daemon's deployment.
  `<project>` is the project token of the daemon, for example `infra`.

- `legion` is the name of the daemon's container.
  `-c legion` selects the log of that container.

- `<from>` and `<to>` are times in RFC 3339, for example `2026-11-14T09:00:00Z`.
  The window starts at `<from>` and stops before `<to>`.
  Give the same `<from>` to `--since-time`, so that `kubectl` sends the lines from that time.

- `--claim <token>` is optional.
  With it, the command reads the lines of one claim only.
  A claim token is `legion-<project>-<issue key in lower case>-<role>`, for example `legion-infra-infra-2811-implementer`.

The command reads the log on its standard input.
It writes the figures on its standard output.

## How far back the reading goes

The command reads only the log that the node keeps for the daemon's container, `legion`.
The node keeps a limited part of that log, and a busy daemon fills it quickly.
The reading thus goes back only as far as the node keeps the log of that container.
Each figure is a lower bound: the true count can be higher.

`kubectl logs` reads the log of the current container only.
When the container restarted after `<from>`, the log of the container before the restart is not in the reading.
To read that log, run the command a second time with `--previous` after `-c legion`.
When Kubernetes replaces the daemon's pod, the new pod starts with an empty log.
`--previous` does not read the log of a pod that Kubernetes replaced.

The second line of the output gives the time of the first line in the window.
Compare that time with `<from>`.
A first line long after `<from>` can show that the node did not keep the start of the window.

## The figures

The output gives one figure on each line, in this order.
A task is a number of one claim: each claim counts its task numbers from 1.
So each figure that names a task names its claim too.

- `window` is the window and the claims that the command read.

- `daemon lines in the window` is the number of the daemon's log lines in the window, with the times of the first and the last.
  With `--claim`, only the lines of that claim count.

- `turns started` and `turns ended` are the workers' turns, by cause.

  - A turn whose cause is a task number is the turn of an accepted take.
    It is the first turn that starts after the daemon accepts the worker's take of that task.

  - A turn with no cause is the turn of a message, or the worker's own turn.

  - The daemon writes `supervise: a turn started` and `supervise: a turn ended` for each turn.
    The `task` attribute of these lines is the cause.
    A line without a `task` attribute is a turn with no cause.

  - The worker's report of a turn writes one more `supervise: a turn ended` line, with a `sequence` attribute.
    The command does not count that line.

- `turns each number took` is, for each task, the number of turns that started with that task as their cause.
  The expected value is 1.

- `numbers written by origin` is the number of tasks that the daemon wrote as values, by the origin of each task.

  - Each task has one origin: `phase-start`, `resume`, `silence`, `wake`, `notice`, `operator`, `check-look` or `controller-start`.

  - The daemon can write the value of one task more than one time.
    The command counts each task one time.

- `numbers written to a claim whose phase waited, by origin` is the waited figure.

  - It counts the tasks written to a claim that had finished the last task written to it, in the session that the claim runs.
    That worker had done its work, and its phase was waiting.

  - The daemon gives this fact as `finished` set to `true` on the line `supervise: a task value was written`.

  - The command counts each task one time, by origin.

- `silence repair` is the number of tasks of origin `silence`, and how many of them went to a claim whose phase waited.

  - The silence repair writes a task to a worker that stayed silent past its bound when the worker's own report does not explain why.

  - The command counts these tasks apart from the tasks of origin `resume`.
    The second verdict does not read them.

- `numbers taken` is the number of tasks that a worker took: the daemon accepted the take.
  A worker can send a take again when it did not get the answer.
  The daemon answers that take again, with `repeat` set to `true`, and the command does not count it.

- `numbers finished, by what finished each` is the number of tasks that finished.

  - `report` is a worker's report of the task's turn, which named the task.

  - `end-frame` is the end of the task's turn, when no report named the task in time.

- `charges by wait` is the number of charges against the workers' prompt budget, by wait.

  - `take`: the worker saw its task and did not take it within the first bound, `worker_take_timeout_seconds`.

  - `turn-start`: the daemon accepted the take, and no turn started within the second bound, `worker_turn_start_timeout_seconds`.

  - `adoption`: the worker's shim refused to adopt the issue's working copy for the task.

- `retirements` is the number of charges that retired a worker's process.
  The daemon retires the process at the worker's limit of charges.

- The three `verdict` lines each give `pass` or `fail`.
  A verdict that fails lists each task that fails it, below it.

  1. `no charge of a worker whose turn began within the second bound after its take`.
     It fails for a charge made while an accepted take stood, if a turn of that claim started within the second bound after that take.

  2. `no write of origin resume to a claim that finished its last task in its session`.
     It fails for each task of origin `resume` written with `finished` set to `true`.

  3. `each number taken once for each time the record owed it`.
     The daemon owes a task one time when it writes the task.
     Each take-back of the task opens one more time owed: the daemon writes `supervise: a task was taken back`.
     The verdict fails for a task taken more times than the daemon owed it.

## An example

The repository holds a sample of the daemon's log: `packages/daemon/internal/supervise/testdata/task-cost.log`.
A test reads this page and runs each command below over that sample.
The test fails when the output is not the output on this page.

This command reads the whole sample, from the repository's root:

```sh
legion task-cost --from 2026-11-14T09:00:00Z --to 2026-11-14T10:00:00Z < packages/daemon/internal/supervise/testdata/task-cost.log
```

```text
window: 2026-11-14T09:00:00Z to 2026-11-14T10:00:00Z, every claim
daemon lines in the window: 36, the first at 2026-11-14T09:00:05.000Z, the last at 2026-11-14T09:55:30.000Z
turns started: 5 (by a task number 4, with no cause 1)
turns ended: 4 (by a task number 3, with no cause 1)
turns each number took:
  legion-infra-infra-2811-implementer task 1: 1
  legion-infra-infra-2811-implementer task 2: 1
  legion-infra-infra-2811-tester task 4: 1
  legion-infra-infra-2811-tester task 5: 1
numbers written by origin: phase-start 2, silence 1, wake 1 (4 in all)
numbers written to a claim whose phase waited, by origin: silence 1, wake 1 (2 in all)
silence repair (origin silence), counted apart from resume: numbers written 1, to a claim whose phase waited 1
numbers taken: 4
numbers finished, by what finished each: end-frame 1, report 2 (3 in all)
charges by wait: take 1, turn-start 1 (2 in all)
retirements: 0
verdict: no charge of a worker whose turn began within the second bound after its take: pass
verdict: no write of origin resume to a claim that finished its last task in its session: pass
verdict: each number taken once for each time the record owed it: pass
```

The sample shows these points:

- The sample holds a line before `<from>` and a line at `<to>`.
  The command counts neither of them.

- The tester took task 4 two times, and the third verdict passes.
  No turn started within the second bound, so the daemon charged the tester and took the task back.
  The take-back opened a second time owed.

- The tester sent its take of task 4 again, and the daemon answered it with `repeat` set to `true`.
  The command does not count that take.

- The silence repair wrote task 2 to the implementer, which had finished task 1 in its session.
  The waited figure counts that task under `silence`, and the second verdict passes.

This command reads one claim of the sample:

```sh
legion task-cost --from 2026-11-14T09:00:00Z --to 2026-11-14T10:00:00Z --claim legion-infra-infra-2811-implementer < packages/daemon/internal/supervise/testdata/task-cost.log
```

```text
window: 2026-11-14T09:00:00Z to 2026-11-14T10:00:00Z, claim legion-infra-infra-2811-implementer
daemon lines in the window: 17, the first at 2026-11-14T09:00:05.000Z, the last at 2026-11-14T09:41:05.000Z
turns started: 3 (by a task number 2, with no cause 1)
turns ended: 3 (by a task number 2, with no cause 1)
turns each number took:
  legion-infra-infra-2811-implementer task 1: 1
  legion-infra-infra-2811-implementer task 2: 1
numbers written by origin: phase-start 1, silence 1 (2 in all)
numbers written to a claim whose phase waited, by origin: silence 1 (1 in all)
silence repair (origin silence), counted apart from resume: numbers written 1, to a claim whose phase waited 1
numbers taken: 2
numbers finished, by what finished each: end-frame 1, report 1 (2 in all)
charges by wait: take 1 (1 in all)
retirements: 0
verdict: no charge of a worker whose turn began within the second bound after its take: pass
verdict: no write of origin resume to a claim that finished its last task in its session: pass
verdict: each number taken once for each time the record owed it: pass
```
