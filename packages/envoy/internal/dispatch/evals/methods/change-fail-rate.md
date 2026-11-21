**How it is measured.** A flag marks a pull request that broke production, and names the pull request that fixed or reverted it. The holder of the dora role confirms or rejects each flag. Until then the flag is pending. A flag counts only when its broken pull request is a population pull request. For each UTC day, `measures.ChangeFailureRateDay` builds three shares:

- `per_deploy`: the deploys that shipped the broken pull request of a confirmed flag, each deploy once, over the successful deploys of the day. The deploys of pending flags and of rejected flags are counted beside it.
- `upper_bound`: the deploys of a confirmed or a pending flag, each deploy once, over the same deploys. It is the rate if every pending flag were confirmed.
- `per_pr`: the confirmed flags over the population pull requests that merged that day. The pending flags, the rejected flags and the confirmed revert flags are counted beside it.

A flag counts on the day its broken pull request deployed. While that pull request has not deployed, the flag counts on the day it merged. `measures.FoldChangeFailureRate` adds up the numerators and the denominators of a window's days and divides the sums. It never takes the mean of the daily shares.

A population pull request is a merged pull request that the delivery measures count (`delivery.IsPopulationPR`): its author is a population author, its repository is not an excluded one, and it is not a task pull request of the deploy repository.

**No data yet.** No failure source exists yet. This eval has no day rows, so it reads "no data". dispatch://INFRA-2917 adds the failure source.

**What it does not count.** Rejected flags, flags whose broken pull request is not a population pull request, and incidents, which have no source yet.
