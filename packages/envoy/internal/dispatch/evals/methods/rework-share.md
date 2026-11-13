**How it is measured.** For each UTC day, `measures.ReworkShareDay` takes the population pull requests that merged that day. It counts the ones whose title starts with the word fix, revert or hotfix, in any case (`delivery.IsRework`), over all of them. `measures.FoldReworkShare` adds up the two counts of a window's days and divides the sums. It never takes the mean of the daily shares. The Delivery page and this eval call the same fold.

A population pull request is a merged pull request that the delivery measures count (`delivery.IsPopulationPR`): its author is a population author, its repository is not an excluded one, and it is not a task pull request of the deploy repository. It counts for the project of the issue it names, or else for the project of its repository.

**What it does not count.** Pull requests outside the population, and fixes whose title does not start with one of the three words.
