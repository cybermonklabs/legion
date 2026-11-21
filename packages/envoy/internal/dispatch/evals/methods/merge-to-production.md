**How it is measured.** For each UTC day, `measures.MergeToProductionDay` takes the population pull requests that merged that day. For each one that deployed, it takes the minutes from its merge to its deploy. A pull request deploys with the first deploy run, created after its merge, that applies it to production (`delivery.ContainingRun`). Its deploy time is the completion of that run's production job. `measures.FoldLeadTime` merges the minutes of every day in a window. It answers their count, median, 90th percentile and maximum (`measures.Quantile`), and never the mean of the daily medians. The Delivery page, the Pipeline section and this eval call the same fold.

A population pull request is a merged pull request that the delivery measures count (`delivery.IsPopulationPR`): its author is a population author, its repository is not an excluded one, and it is not a task pull request of the deploy repository. It counts for the project of the issue it names, or else for the project of its repository.

The target is under 45 minutes for every change, so it judges the maximum and the median.

**What it does not count.** Pull requests that have not deployed, pull requests outside the population, and the time before the merge.
