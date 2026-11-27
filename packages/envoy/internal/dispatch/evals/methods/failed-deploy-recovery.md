**How it is measured.** A flag marks a pull request that broke production, and names the pull request that fixed or reverted it. For each UTC day, `measures.TimeToRestoreDay` takes the confirmed flags whose broken pull request deployed that day. For each flag whose fix is a population pull request, and whose two pull requests both deployed, it takes the minutes from the deploy of the broken pull request to the deploy of the fix. `measures.FoldTimeToRestore` merges the minutes of a window's days and answers their median (`measures.Quantile`). It never takes the mean of the daily medians.

A population pull request is a merged pull request that the delivery measures count (`delivery.IsPopulationPR`): its author is a population author, its repository is not an excluded one, and it is not a task pull request of the deploy repository.

**No data yet.** No failure source exists yet. This eval has no day rows, so it reads "no data". dispatch://INFRA-2917 adds the failure source.

**What it does not count.** Pending flags, rejected flags, fixes that are not population pull requests, and failures whose fix has not deployed.
