**How it is measured.** For each UTC day, `measures.DeployRunSuccessDay` reads the runs of the deploy workflow on main that started that day. The denominator is the runs that concluded success or failure. The numerator is those of them whose production job concluded success. A cancelled run is no deploy attempt, so the share leaves it out and counts it beside the share, in `cancelled`. `measures.FoldDeployRunSuccess` adds up the numerators and the denominators of a window's days and divides the sums. It never takes the mean of the daily shares. The Delivery page and this eval call the same fold.

A run counts for each project whose pull requests it ships. A run that ships no pull request counts for the project of the deploy repository.

**What it does not count.** Cancelled runs, runs that are still running, runs with any other conclusion, runs on other branches, and runs of other workflows.
