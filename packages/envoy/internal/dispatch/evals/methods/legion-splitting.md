**How it is measured.** A filing is an issue that a Legion session created. `legionfacts` stores each filing with its class, by the rules of `legion/metrics`:

- `by_the_controller`: the controller filed it.
- `in_tree_at_design`: a worker filed it into the tree of its own issue before that issue had its first implementer event.
- `in_tree_later`: a worker filed it into the tree of its own issue after that event.
- `out_of_tree`: a worker filed it anywhere else.

The tree of a filing starts at the parent the issue had at its `issue.created` event, and climbs through the parent of each issue to the top. A filing is in the filer's tree when that climb and the climb from the filer's issue reach the same issue.

For each UTC day, `metrics.SplittingDay` builds these series:

- `filed:<class>`: the filings of each class, on the day they were made.
- `finished`: the issues that finished, on their close day, as legion-work-time counts them.
- `roots`: the roots that finished, with their children counted beside.
- `hours_with_children` and `hours_without_children`: the work time of each finished root with children, and of each one with none.

`metrics.FoldSplitting` merges a window's days. It answers the filings of each class and the issues that finished, in all and by UTC week, a week starting on a Monday. It also answers the roots, their children, the children per root, and the median and 90th percentile of the two work times.

The filings and `finished` have their roots cut and their children cut, read by the tree that the filed or finished issue is in now. The root figures count roots alone, so their roots cut is the same as their all cut, and they have no children cut.

**What it does not count.** Issues that a person, or a session that is not a Legion session, created. The root figures count no open root.
