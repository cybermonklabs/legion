**How it is measured.** For each UTC day, `measures.DeployFrequencyDay` reads the runs of the deploy workflow on main that started that day. It counts each run whose production job concluded success. A run that reached production still counts when it was cancelled after that. For each counted run it also counts, in `with_prs`, whether the run shipped a pull request. `measures.FoldDeployFrequency` adds up the days of a window. It divides each sum by the length of the window in days, which gives `per_day` and `with_prs_per_day`. The Delivery page and this eval call the same fold.

A deploy counts for each project whose pull requests it ships. A deploy that ships no pull request counts for the project of the deploy repository.

**The rule for `with_prs` changed.** dispatch://INFRA-2916 changed which deploys count as deploys with pull requests.

- The old rule: a deploy counted when it shipped a pull request that merged inside the reader's window.
- The new rule: a deploy counts when it shipped any pull request, whenever that pull request merged. A pull request ships with the first deploy run, created after its merge, that applies it to production (`delivery.ContainingRun`).
- Why it changed: a day row holds one value, and every window that contains the day reads that value. A rule that depends on the reader's window cannot be a part of one day.
- The measured difference, over the golden delivery history: the Delivery page's count moved by one deploy in 4 of 20 windows. The comparison of the day rows with the Delivery routes measured one deploy in 5 of 10 windows. A deploy moves when every pull request it shipped merged before the window started. The old rule counted such a deploy as one with no pull request.

**What it does not count.** Runs on other branches, runs of other workflows, and runs whose production job failed, was skipped or did not run.
