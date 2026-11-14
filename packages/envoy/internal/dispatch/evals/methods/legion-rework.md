**How it is measured.** The eval counts the project's issues that finished in the window, as legion-work-time counts them. It reads three values of each issue, which `legionfacts` stores by the rules of `legion/metrics`:

- Review rounds: the entries into `reviewing`. Before the daemon's observations of the issue, they are the entries into the status `needs_review`.
- Sent back: whether the issue moved to `implementing` from `testing`, `reviewing`, `accepting` or one of the merger's phases (`retro`, `merging` and `awaiting_merge`). Before the daemon's observations, the history of the issue's statuses answers the same question.
- Hours after the first review: the implementing and testing hours from the first review on.

For each UTC day, `metrics.ReworkDay` builds three series from the issues that finished that day: `review_rounds`, the rounds of each issue, `sent_back`, the issues sent back over the issues that finished, and `hours_after_first_review`, the hours of each issue. `metrics.FoldRework` merges a window's days. It answers the count, median and 90th percentile of the review rounds, the share sent back from the summed counts, and the sum, count, median and 90th percentile of the hours after the first review. Each series has its roots cut and its children cut. A target on this eval judges `review_rounds.median`.

**What it does not count.** Open issues, the rounds of the other issues of the tree, and the hours before the first review.
