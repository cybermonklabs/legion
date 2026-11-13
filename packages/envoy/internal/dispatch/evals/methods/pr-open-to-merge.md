**How it is measured.** For each UTC day, `measures.OpenToMergeDay` takes every population pull request that merged that day, deployed or not. It takes the minutes from the moment GitHub records for the pull request's opening to its merge. `measures.FoldLeadTime` merges the minutes of every day in a window. It answers their count, median, 90th percentile and maximum (`measures.Quantile`), and never the mean of the daily medians. The Pipeline section and this eval call the same fold.

A population pull request is a merged pull request that the delivery measures count (`delivery.IsPopulationPR`): its author is a population author, its repository is not an excluded one, and it is not a task pull request of the deploy repository. It counts for the project of the issue it names, or else for the project of its repository.

The target is a median under 60 minutes.

**What it does not count.** Pull requests that closed with no merge, and pull requests outside the population.
