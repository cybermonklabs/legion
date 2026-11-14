**How it is measured.** For each UTC day, `measures.DeployJobsDay` reads the runs of the deploy workflow on main that a push started on that day. This eval follows the job that the delivery settings name as the staging end-to-end gate. Each run adds at most one result: the outcome of that job, and its minutes from the job's start to its completion when it ended in success or failure. A timed-out job counts as a failure. A cancelled job is counted beside the times and is not timed. `measures.FoldTiming` merges the minutes of a window's days. It answers their count, their median and their 90th percentile to one decimal, the count of each outcome, and the success share: the successes over the successes and the failures. The Pipeline section and this eval call the same fold.

Dispatch writes day rows of this eval only when the delivery settings name the gate's job. When two deploy cards follow one job name, the job counts for the first of them, in this order: the production deploy, the staging end-to-end gate, the staging deploy.

A run counts for the project of its repository.

**What it does not count.** Runs in which the job did not run or is still running, runs on other branches, and runs of other workflows.
