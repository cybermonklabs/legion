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
kubectl -n legion logs deploy/legion-<project>-daemon -c legion | legion task-cost --from <from> --to <to> [--claim <token>] [--take-timeout <seconds>]
```

- `legion-<project>-daemon` is the daemon's deployment.
  `<project>` is the project token of the daemon, for example `infra`.

- `legion` is the name of the daemon's container.
  `-c legion` selects the log of that container.

- `<from>` and `<to>` are times in RFC 3339, for example `2026-11-14T09:00:00Z`.
  The window starts at `<from>` and stops before `<to>`.
  Every figure counts only what happened in the window.

- The command does not give `--since-time` to `kubectl`.
  So `kubectl` sends the whole log of the current container, from the start of the container.
  The command reads the lines before `<from>` only to know where each claim and each task stands at `<from>`.
  For example, take a task that the daemon wrote and the worker saw before `<from>`, and that no take reaches in the window.
  No `task` attribute in the window names that task, unless its first bound passes inside the window while a hold stands over it.
  The daemon then writes `supervise: the first bound passed while a hold stands over the task; the worker is not charged`, with the `task` attribute.
  Without a hold, the daemon charges the worker when the first bound passes, with `supervise: prompt failure`, or `supervise: retired after prompt failures` at the limit.
  That line names the number of the task only in the text of its `why` attribute, for example `task 3`.
  Without the hold's line, a reading of the window alone does not see that task.

- `--claim <token>` is optional.
  With it, the command counts the lines of one claim only.
  It still reads the time of every line of any claim that the command decodes (a turn, a task value, a take, a finish, a charge, a death, a registration or a view line) and each start of the daemon, since those bound every claim's clock.
  A claim token is `legion-<project>-<issue key in lower case>-<role>`, for example `legion-infra-infra-2811-implementer`.

- `--take-timeout <seconds>` is optional.
  It is the first bound of the deployment, `worker_take_timeout_seconds`, in seconds.
  Without it, the command uses 300, the default of that setting.
  Give the value that the `legion.yaml` of the deployment sets.
  No line that the daemon always writes gives this value, so the command cannot read it from the log.

The command reads the log on its standard input.
It ignores each line at `<to>` or after it.
It writes the figures on its standard output.

The command exits with status 0 when it judges every verdict, also when a verdict fails.
It exits with status 1 when any verdict gives `no evidence`, and when it cannot read its standard input.
No check can pass a reading in which a verdict has no evidence.
Each verdict gives `no evidence` in a window with no line of the daemon, or with no line of the claim that `--claim` names.
A verdict also gives `no evidence` in a window whose lines hold no task value written and no take answered, unless a fail that the state at `<from>` proves fails that verdict, as the verdicts below say.
So the status says whether the command could judge the window, and the verdict lines say what the command found.
It exits with status 2 when it cannot use its arguments.
It then names the argument that it cannot use, and `legion task-cost --help`, which writes its usage.

## How far back the reading goes

The command reads only the log that the node keeps for the daemon's container, `legion`.
The node keeps a limited part of that log, and a busy daemon fills it quickly.
The reading thus goes back only as far as the node keeps the log of that container.
Each figure is a lower bound: the true count can be higher.
There are two exceptions, which can also count too many tasks or turns.
The first is `numbers owed that no take reached within the first bound`, and its entry below says when.
The second is the turns that each task number took.
The daemon can give a turn to a take inside the window and then put the take back at or after `<to>`.
The reading reads no line from `<to>` on, so it counts that turn as a turn of the number.

`kubectl logs` reads the log of the current container only.
So a reading goes back no further than the start of the current container.
When the container restarted after `<from>`, the log of the container before the restart is not in the reading.
To read that log, run the command a second time with `--previous` after `-c legion`.
`--previous` reads the log of the container before the current one.
When Kubernetes replaces the daemon's pod, the new pod starts with an empty log.
`--previous` does not read the log of a pod that Kubernetes replaced.

The log of one container holds the state of each task at `<from>`.
After a start, the daemon writes again each value that a worker saw before the start, when the first bound passes.
The daemon writes again each value that nobody saw, one time in each first bound.

The second line of the output gives the time of the first line in the window.
Compare that time with `<from>`.
A first line long after `<from>` can show that the node did not keep the start of the window.

## The figures

The output gives one figure on each line, in this order.
A task is a number of one claim: each claim counts its task numbers from 1.
So each figure that names a task names its claim too.

- `window` is the window, the claims that the command read, and the first bound that it used.

- `daemon lines in the window` is the number of the daemon's log lines in the window, with the times of the first and the last.
  With `--claim`, only the lines of that claim count.
  When the window holds no line of the daemon, the line is only `daemon lines in the window: 0`, with no times, and no line about the evidence follows it.
  When the window holds lines of the daemon but they hold no task value written and no take answered, the next line says so.
  It is `task values written and takes answered in the window: 0, so no verdict is judged` when each verdict gives `no evidence`.
  It is `task values written and takes answered in the window: 0, so only a fail carried into the window is judged` when a fail that the state at `<from>` proves fails a verdict.
  Each verdict that such a fail fails gives `fail`, and each other verdict gives `no evidence`.

- `turns started` and `turns ended` are the workers' turns, by cause.

  - A turn whose cause is a task number is the turn of an accepted take.
    It is the first turn that starts after the daemon accepts the worker's take of that task.

  - A turn with no cause is the turn of a message, or the worker's own turn.

  - The daemon writes `supervise: a turn started` when a turn starts.
    It writes `supervise: a turn ended` when the worker's agent says that the turn ended.
    The `task` attribute of these lines is the cause.
    A line without a `task` attribute is a turn with no cause.

  - A turn that the death of its process cuts off has no `supervise: a turn ended` line, and neither has a turn that the daemon cuts off when it stops the process and takes the task back.
    So `turns ended` can be smaller than `turns started`.

  - The worker's report of a turn writes one more `supervise: a turn ended` line, with a `sequence` attribute.
    The command does not count that line.

  - The daemon gives the first turn after an accepted take to that take.
    When the worker's report of that turn names no task, that turn was the turn of something else.
    The daemon then puts the take back to wait for its own turn, and writes `supervise: a turn's report names no task of the take whose turn it was given; the take awaits its turn again`.
    The command counts the turn that the daemon put back as a turn with no cause, at its start and at its end, although these two lines name the task.
    The worker's report says this for certain when it names the take as `held`: the take's text was still waiting, unrun, when the turn ended.
    After a daemon restart under a live worker, the daemon has lost its count of the worker's turns, and such a report is the only one that puts the first take back.
    The daemon loses that count in the same way when the worker's shim reconnects with a hello that does not say that its shim dropped no frame while no daemon was connected, because a turn that the shim dropped whole would leave the count one turn short, and it writes `supervise: the shim's hello does not say its backlog is whole; the count of its process's turns is no longer known`.
    A take that the daemon accepted before the restart is put back the same way, unless the daemon before the restart saw the end of the turn it gave that take: the daemon after it cannot tell then whether the take's own turn also ran before the restart, and that take's report puts nothing back.
    A take that the daemon accepted before the restart is not put back either when the worker's first hello after the restart does not say that its shim dropped no frame while no daemon was connected, or when the worker does not answer on that connection: frames lost in the restart could hide the take's own turn, and the daemon writes `supervise: the first hello after the daemon start does not show its connection carries every frame its shim held; no report gives the restored take its turn back`.
    When a `held` report arrives before the start of its turn, the daemon gives that turn no take, and writes `supervise: a turn started that the producer said ran with the accepted take's text held; the turn is no task's, and the take awaits its turn`.
    The command counts that turn as a turn with no cause, since its lines name no task.
    A take whose turn the daemon counted ended, and whose report naming its task never came, is taken back when no report of it comes within the second bound, `worker_turn_start_timeout_seconds`, from the end of the last turn that ran.
    That happens when the worker's host dropped the task's text, so the turn the daemon gave the take reported no task, and no report puts the take back.
    The daemon writes `supervise: the report of the take's turn did not come by its bound; the task is taken back with the note of a cut turn, and what waited for the report runs or its value is written again` and `supervise: a task was taken back`, writes the value again under the same number, and charges nothing.
    The bound runs only while the worker's connection shows the daemon every frame of its turns: a closed connection stops it, and it does not run while no connection stands.
    After the shim's next hello, and after a daemon restart, it starts again whole at the agent's answer to the daemon's ask whether it is in a turn, or at a turn's start or end on the new connection, whichever comes first, and a take or a report that reaches the daemon before that hello starts none.
    After a daemon restart it also leaves alone a take that the daemon accepted before the restart unless the daemon would put that take back by the rules above: frames lost in the restart could hide the take's own turn, and the bound would end that turn.

- `turns each number took` is, for each task, the number of turns that started with that task as their cause.
  A turn that the daemon put back does not count.
  The expected value is 1.
  After a hello that does not say that the worker's shim dropped no frame, a task can take 0 turns.
  The shim can have dropped the whole turn of the task.
  Also, after such a hello, while the daemon does not know the count of the worker's turns, when the worker's report of a turn arrives before the start of that turn, the daemon does not wait for that start, so a start that does come names no task.

- `numbers written by origin` is the number of tasks that the daemon wrote as values, by the origin of each task.

  - Each task has one origin: `phase-start`, `resume`, `silence`, `wake`, `notice`, `news`, `operator`, `check-look` or `controller-start`.
    `news` is a task queued for news of the approved design alone: it tells the worker what changed in the design its work stands on and gives it no work of its phase, so the command counts it apart from the phase's work.

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

- `numbers owed that no take reached within the first bound` counts each time that the daemon owed a worker a task and the worker did not take the task within the first bound, `worker_take_timeout_seconds`, of the daemon's first wait.
  Below it, one line gives each such time with its claim and its task: when the clock of the task last started, when the bound passed, and what came after.
  This is a stranded task, which the design of task delivery forbids.
  The third verdict fails for each of these times too.
  The production check of task delivery, check c9, reads this figure as **zero**.

  - The daemon owes a task from the time that it writes the task, unless a take of the task that the daemon accepted still stands.
    The daemon stops owing the task when it accepts a take of the task, when it answers a take `dropped`, when the task finishes, when its value is deleted, and when the daemon writes a newer task to the claim.

  - A take-back of the task, `supervise: a task was taken back`, makes the daemon owe the task again, as a new time owed.
    This is true also when the daemon does not write the task again.
    For example, the daemon takes the task back from a process that it stopped, and the next process of the worker reads the task as the daemon last wrote it.

  - The clock of the task runs only during the daemon's first wait: the time in which the worker has seen the task and can take it.
    The clock starts at the line `supervise: a task value was reported seen` for the latest value that the daemon wrote for the task.
    That line must give the same `revision` as the line of the write, and also the same `epoch` when it gives an `epoch`.
    The epoch changes when the task bucket is created again, and the revisions then start again at 1.
    A new write of the task stops the clock until the worker reports the new value seen.
    A report of another value also stops the clock, for example a late report of a value from the bucket before it was created again.

  - A report of a seen task whose write is not in the log opens the time owed at that report, with the `epoch` and `revision` that the report gives.
    This happens when the daemon wrote the task before the first line that the node kept.

  - A turn of the worker, `supervise: a turn started`, stops the clock.
    The end of that turn, `supervise: a turn ended`, starts the clock again, and the bound starts again from zero.
    A take-back of the task also ends the turn of the claim.

  - A take that the daemon answered `refused` stops the clock until the daemon writes the task again, for example when the hold ends.
    The clock then starts again at the next report that the worker saw the task.
    A take that the daemon could not decide or record (`supervise: a take could not be decided or recorded; …`) and a take-back of the task stop the clock in the same way.
    The next registration of the worker's agent also ends each of these stops.

  - The death of the worker's process, `supervise: process died`, stops the clock until the next registration of the worker's agent, `supervise: an agent registered`.

  - A registration is the agent of a new process, which has not seen the task yet.
    The daemon does not charge a new process for a task that only the process before it saw.
    So after a registration, the clock starts again only at the report of the new process that it saw the task.

  - The clock runs only while the daemon sees every frame of the worker's turns on the worker's connection.
    The daemon writes `supervise: the daemon no longer sees every frame of the worker's turns; the bounds on the worker stand still until it does again` when it stops seeing them: when the connection closes, when a start of the daemon restores a claim with a process, and when a hello over a pending task comes on a new connection before the daemon read the close of the one before it.
    That line stops the clock, and no act of the worker starts it again.
    The daemon writes `supervise: the daemon sees every frame of the worker's turns again; the bounds on the worker run again, whole, from here` when it sees them again: at the first hello of a new process, at a hello of a worker whose agent has not said it is ready yet, at a hello over no pending task, at the agent's answer to the daemon's ask whether it is in a turn, at a turn's start or end, and when the daemon lets the worker's process go.
    That line starts the clock again, and the bound starts again from zero.

  - A start of the daemon stops the clock at the time of the last line of the daemon before it that the command decodes: a turn, a task value, a take, a finish, a charge, a death, a registration or a view line.
    No time between the two daemons counts, because the new daemon runs none of its waits until the worker acts and the daemon sees every frame again.
    The daemon writes `legion daemon starting`, with the `project`, as its first line.
    Only the configuration warnings of `legion start` come before it.
    The command decodes no warning, so a warning is not a line of the daemon before, and its time stops no clock.
    So the command reads each line that it decodes before `legion daemon starting` as a line of the daemon before, and each line after it as a line of the new daemon.
    Without `--claim`, the command still counts each warning in the window as a line of the window, so the time of a warning can be the time of the first line in the window that the output gives.
    The new daemon then restores its claims and writes `legion daemon started`.
    The clock of a claim that it restores with a process starts again at the line that starts it, before `legion daemon started` or after it.
    The clock of every other claim starts again at the worker's next act that the command reads in the log: a report that it saw the task, a take, a turn, a report that put a take back, or a registration.
    In a log that holds no `legion daemon starting` line, the log of an older daemon, `legion daemon started` marks the start.
    A daemon that starts with a report of a seen task from its database writes the task again at the first bound, and charges nobody.
    So a clock whose bound ends at a new write of the task has not passed the bound.

  - Only the time of the clock in the window counts.
    A clock that runs at `<from>` counts from `<from>`, as if the worker saw the task at `<from>`.
    So a task that the worker saw before `<from>` and did not take is listed when one first bound after `<from>` passes before `<to>`.
    A task that the worker took soon after `<from>` is not listed, however long it waited before `<from>`.

  - A task whose clock has not passed the bound at `<to>` is not listed, because the window does not show whether a take came in time.

  - A task can be listed more than one time: one time for each time that the daemon owed it.

  - The figure can count too many tasks.
    The log has no line for the time that a new process of the worker needs to start after a release, a suspension or a relaunch, for the time of a compaction, or for a hold that stands while the worker asks for nothing.
    When the daemon lets a process go, it writes the line that starts the clock for the next process, so a clock that a closed connection stopped runs during that time.
    A turn that started before the first line of the log has no start line in the log.
    The clock counts all of that time, so the figure can list a task that no worker could take in time.

  - The figure can also miss a task.
    When the daemon stops a process during a turn for a reason other than a death, and takes back no task, the log has no line for the end of that turn.
    The clock then starts again only at the end of the next turn of that claim, or at the first report of the claim's next process that it saw the task.
    A take-back of a task that the daemon last wrote before the first line of the log opens no time owed that the command can measure until a report of the task seen is in the log, because the command does not know which revision the worker must report seen.

- `numbers finished, by what finished each` is the number of tasks that finished.

  - `report` is a worker's report of the task's turn, which named the task.

  - `end-frame` is the end of the task's turn, when no report named the task in time.
    Only a worker whose plugin takes no task, a contract below 306 (`supervise.TaskTakeContract`), has its task finished this way: such a worker names no task in its report.
    A current worker's task finishes only by the report that names it.

- `charges by wait` is the number of charges against the workers' prompt budget, by wait.

  - `take`: the worker saw its task and did not take it within the first bound, `worker_take_timeout_seconds`.

  - `turn-start`: the daemon accepted the take, and no turn started within the second bound, `worker_turn_start_timeout_seconds`.

  - `adoption`: the worker's shim refused to adopt the issue's working copy for the task.

- `retirements` is the number of charges that retired a worker's process.
  The daemon retires the process at the worker's limit of charges.

- The three `verdict` lines each give `pass`, `fail` or `no evidence`.
  A verdict that fails lists each task that fails it, below it.
  The command counts the evidence for each verdict apart.
  So it never gives a pass for which it has no evidence, and never hides a fail that it can prove.

  - The window holds the evidence of the delivery by task values when it holds a line of the daemon, and a line `supervise: a task value was written` or a line `supervise: a take was answered`, with the outcome `accepted`, `refused` or `dropped`.
    With `--claim`, only the lines of that claim count.
    In such a window, each verdict gives `pass` or `fail`.

  - The state at `<from>` is carried into the window.
    The lines before `<from>` give it: a task that the worker saw and did not take, and a take that the daemon accepted.
    This carried state can prove two fails in the window:

    - A task that the worker saw before `<from>` and did not take, when one first bound after `<from>` passes before `<to>`.
      `numbers owed that no take reached within the first bound` lists it, and the third verdict fails for it.

    - A charge in the window of a worker whose turn began within the second bound after a take that the daemon accepted before `<from>`.
      The first verdict fails for it.

    A fail that the state at `<from>` proves is evidence for its own verdict only.
    In a window that holds a line of the daemon but not the evidence of the delivery by task values, each verdict that such a fail fails gives `fail`, and each other verdict gives `no evidence`.
    A pass that the state at `<from>` shows is not evidence.
    A bound that passed before `<from>`, or a charge made before `<from>`, is no evidence for the window.

  - Each verdict gives `no evidence` when the window holds no evidence for it.
    Two kinds of window give `no evidence` for all three verdicts:

    - The window holds no line of the daemon, or no line of the claim that `--claim` names.
      A wrong selector in the `kubectl` command, or a window that the node no longer keeps, gives this result.
      Such a window gives no fail either, because no line shows that the daemon ran in the window.

    - The window holds lines of the daemon, but no task value written, no take answered, and no fail that the state at `<from>` proves.
      The log of a daemon from before the delivery by task values gives this result: its turns have no cause, and its charges are for prompts that the daemon sent.
      The log of a daemon that is not rolled out yet gives this result, and so does the log of an older container that `--previous` reads.
      Such a log does not show how the daemon delivers a task by its value, so check c9 must not read it as three passes.
      Both fails that the state at `<from>` can prove need lines of the delivery by task values before `<from>`, so these logs give no fail.

  A verdict with no evidence neither passes nor fails, and the command exits with status 1 when any verdict gives `no evidence`.
  The output still gives every other figure as the command read it.

  1. `no charge of a worker whose turn began within the second bound after its take`.
     It fails for a charge made while an accepted take stood, if a turn of that claim started no earlier than the start of the second wait of that take, and no later than the charge's limit.
     The second wait begins at the latest of these times: the daemon accepts the take; the daemon puts the take back; a turn that ran while the take waited for its turn ends; the daemon sees every frame of the worker's turns again after a closed connection or a start of the daemon, `supervise: the daemon sees every frame of the worker's turns again; …`.
     So a turn that the daemon gave to the take and then put back is not a turn within the second bound.
     For a `turn-start` charge, the limit is the second bound after the second wait began.
     For every other charge, the limit is the charge itself.
     When the second wait began after the take, the line of the failure gives that time too.
     A take that the worker makes while the daemon does not see every frame of its turns, for example before the agent answers the daemon's ask after the shim's hello, begins its second wait at that line.
     After a start of the daemon, a claim with neither line about what the daemon sees has no process, and its second wait begins again at the worker's first act that the command reads too, as the daemon's waits do.
     So the command's second wait never begins after the daemon's.
     It can begin before the daemon's, when a hold that the command does not read kept the daemon's wait from running, for example while the worker's new process was not ready yet.
     The verdict can then miss a turn that began late in the daemon's bound.

  2. `no write of origin resume to a claim that finished its last task in its session`.
     It fails for each task of origin `resume` written with `finished` set to `true`.

  3. `each number taken once for each time the record owed it`.
     The daemon owes a task one time when it writes the task.
     Each take-back of the task opens one more time owed: the daemon writes `supervise: a task was taken back`.
     The verdict fails for a task taken more times than the daemon owed it.
     It also fails for each time in `numbers owed that no take reached within the first bound`, because the daemon owed that task and no take came in time.

## An example

The repository holds a sample of the daemon's log: `packages/daemon/internal/supervise/testdata/task-cost.jsonl`.
A test reads this page and runs each command below over that sample.
The test fails when the output is not the output on this page.

This command reads the whole sample, from the repository's root:

```sh
legion task-cost --from 2026-11-14T09:00:00Z --to 2026-11-14T10:00:00Z < packages/daemon/internal/supervise/testdata/task-cost.jsonl
```

```text
window: 2026-11-14T09:00:00Z to 2026-11-14T10:00:00Z, every claim, first bound 5m0s
daemon lines in the window: 65, the first at 2026-11-14T09:00:05.000Z, the last at 2026-11-14T09:55:30.000Z
turns started: 8 (by a task number 6, with no cause 2)
turns ended: 7 (by a task number 5, with no cause 2)
turns each number took:
  legion-infra-infra-2811-implementer task 1: 1
  legion-infra-infra-2811-implementer task 2: 1
  legion-infra-infra-2811-reviewer task 1: 1
  legion-infra-infra-2811-tester task 4: 1
  legion-infra-infra-2811-tester task 5: 1
  legion-infra-infra-2812-planner task 1: 1
numbers written by origin: notice 1, operator 1, phase-start 2, silence 1, wake 1 (6 in all)
numbers written to a claim whose phase waited, by origin: notice 1, silence 1, wake 1 (3 in all)
silence repair (origin silence), counted apart from resume: numbers written 1, to a claim whose phase waited 1
news of the approved design (origin news), counted apart from the phase's work: numbers written 0, to a claim whose phase waited 0
numbers taken: 6
numbers owed that no take reached within the first bound: 2
  legion-infra-infra-2811-implementer task 2: the clock started 2026-11-14T09:30:00.000Z, the bound passed 2026-11-14T09:35:00.000Z, taken 2026-11-14T09:35:20.000Z
  legion-infra-infra-2812-planner task 1: the clock started 2026-11-14T09:00:00.000Z, the bound passed 2026-11-14T09:05:00.000Z, taken 2026-11-14T09:06:00.000Z
numbers finished, by what finished each: report 5 (5 in all)
charges by wait: take 2, turn-start 1 (3 in all)
retirements: 0
verdict: no charge of a worker whose turn began within the second bound after its take: pass
verdict: no write of origin resume to a claim that finished its last task in its session: pass
verdict: each number taken once for each time the record owed it: fail
  legion-infra-infra-2811-implementer task 2: owed, and no take within the first bound, which passed 2026-11-14T09:35:00.000Z
  legion-infra-infra-2812-planner task 1: owed, and no take within the first bound, which passed 2026-11-14T09:05:00.000Z
```

The sample shows these points:

- The sample holds two lines before `<from>` and a line at `<to>`.
  The command counts none of them.
  It reads the two lines before `<from>` to know where the planner of INFRA-2812 stands at `<from>`.

- The daemon wrote task 1 to the planner, and the planner saw it, before `<from>`.
  So the figures do not count that write: `numbers written by origin` counts only writes in the window.
  The daemon charged the planner at 09:03:30.500, one first bound after the planner saw the task.
  The planner took task 1 at 09:06:00.
  The clock of task 1 counts only the time in the window, from `<from>`, so the bound passed at 09:05:00.
  The figure of the numbers owed lists task 1, and the third verdict fails for it.
  A reading of the window alone would not list task 1, because the window holds no write and no seen report of it.

- The tester took task 4 two times, and the third verdict does not fail for task 4.
  No turn started within the second bound, so the daemon charged the tester and took the task back.
  The take-back opened a second time owed.

- The tester sent its take of task 4 again, and the daemon answered it with `repeat` set to `true`.
  The command does not count that take.

- The daemon refused the first take of task 4 because of a hold.
  The refusal stopped the clock of task 4 until the daemon wrote task 4 again when the hold ended.
  The tester took task 4 half a second after it reported that new write seen, so task 4 is not in the figure of the numbers owed.

- The reviewer saw task 1, and then ran a turn of its own for nearly eight minutes before it took task 1.
  The turn stopped the clock, so the figure does not count task 1, although the take came more than a first bound after the reviewer saw the task.

- The silence repair wrote task 2 to the implementer, which had finished task 1 in its session.
  The waited figure counts that task under `silence`, and the second verdict passes.

- The implementer saw task 2 when the daemon wrote it, and took task 2 twenty seconds after its first bound passed.
  The daemon charged the implementer for it.
  The figure of the numbers owed lists task 2, with the time of that take, and the third verdict fails for task 2.

- The daemon wrote task 3, a notice, to the implementer after the implementer finished task 2.
  The waited figure counts task 3 under `notice`.
  Nobody reported seeing task 3, so the daemon wrote it again at each first bound and charged nobody.
  The clock of task 3 never started, so the figure does not count it: the relay failed, not the worker.

This command reads one claim of the sample:

```sh
legion task-cost --from 2026-11-14T09:00:00Z --to 2026-11-14T10:00:00Z --claim legion-infra-infra-2811-implementer < packages/daemon/internal/supervise/testdata/task-cost.jsonl
```

```text
window: 2026-11-14T09:00:00Z to 2026-11-14T10:00:00Z, claim legion-infra-infra-2811-implementer, first bound 5m0s
daemon lines in the window: 25, the first at 2026-11-14T09:00:05.000Z, the last at 2026-11-14T09:55:00.000Z
turns started: 3 (by a task number 2, with no cause 1)
turns ended: 3 (by a task number 2, with no cause 1)
turns each number took:
  legion-infra-infra-2811-implementer task 1: 1
  legion-infra-infra-2811-implementer task 2: 1
numbers written by origin: notice 1, phase-start 1, silence 1 (3 in all)
numbers written to a claim whose phase waited, by origin: notice 1, silence 1 (2 in all)
silence repair (origin silence), counted apart from resume: numbers written 1, to a claim whose phase waited 1
news of the approved design (origin news), counted apart from the phase's work: numbers written 0, to a claim whose phase waited 0
numbers taken: 2
numbers owed that no take reached within the first bound: 1
  legion-infra-infra-2811-implementer task 2: the clock started 2026-11-14T09:30:00.000Z, the bound passed 2026-11-14T09:35:00.000Z, taken 2026-11-14T09:35:20.000Z
numbers finished, by what finished each: report 2 (2 in all)
charges by wait: take 1 (1 in all)
retirements: 0
verdict: no charge of a worker whose turn began within the second bound after its take: pass
verdict: no write of origin resume to a claim that finished its last task in its session: pass
verdict: each number taken once for each time the record owed it: fail
  legion-infra-infra-2811-implementer task 2: owed, and no take within the first bound, which passed 2026-11-14T09:35:00.000Z
```
