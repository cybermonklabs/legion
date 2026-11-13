**How it is measured.** The eval counts the project's issues that finished in the window, as legion-work-time counts them. For a role R, the idle time of an issue is the hours of its worked spans in which all three of these are true:

1. The issue holds its own slot, by the daemon's `slot` observations of it.
2. The issue's observed phase is in the bar of R.
3. The newest claim state the daemon observed for R (its `claim.<R>` observations) is not `working`. No claim counts as not working.

The worked spans are the life of the issue less its parked hours and its hours in the queue: the spans that its work time counts. The phase is the observed one alone, so an hour before the daemon first observed the issue is in no bar. The bars are:

- architect: `design` and `accepting`
- planner: `planning`
- implementer: `implementing`
- tester: `testing`
- reviewer: `reviewing`
- merger: `retro`, `merging` and `awaiting_merge`

`legionfacts` stores the idle hours of each issue by role, by the rules of `legion/metrics`. For each UTC day, `metrics.IdleTimeDay` builds one series for each of the six roles: the idle hours in that role of every issue that finished that day, with a zero where it had none. `metrics.FoldIdleTime` merges a window's days. For each role it answers the hours, the count, the median and the 90th percentile. Each series has its roots cut (`<role>:roots`) and its children cut (`<role>:children`).

The day rows start on the first UTC midnight at or after the project's first claim observation. So a window that starts before that observation reads "no data".

**It is not the idle segment of phase time.** The idle segment of legion-phase-time is the hours of a bar that no other segment claimed. Here an hour of the bar of R, with R out of a turn, is idle whether or not CI, a person or a child held the issue in that hour.

**What it does not count.** Hours in which the issue held no slot of its own, which legion-slot-wait counts, parked hours, hours back in the queue, and the phases on no role's bar.
