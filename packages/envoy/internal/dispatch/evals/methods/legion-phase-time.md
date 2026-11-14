**How it is measured.** The eval counts the project's issues that finished in the window, as legion-work-time counts them. It shares the work time of each issue out over seven bars, one for each worker type, by the issue's phase:

- architect: `design` and `accepting`
- planner: `planning`
- implementer: `implementing`
- tester: `testing`
- reviewer: `reviewing`
- merger: `retro`, `merging` and `awaiting_merge`
- no role: `awaiting_person` and `held`

The hours in the queue are on no bar. So the seven bars of an issue add up to its work time, and this eval and legion-work-time agree.

The hours of each bar split into five segments. Each hour goes to the first segment of this list that holds through it:

1. Waiting on a person: the daemon recorded the root in `awaiting_person`, or an ask that a session of the tree opened on the issue was on the person's turn.
2. In a turn: a role of the issue had the claim state `working`.
3. CI wait: the pull request was open, its base branch requires a check, and the daemon had no CI verdict for its head.
4. Waiting on children: the issue is a root with a child in a phase from planning to awaiting merge, during that child's own work time.
5. Idle: every hour that no segment above claimed.

`legionfacts` stores the part of each bar that each issue holds. For each UTC day, `metrics.PhaseTimeDay` builds one series for each bar, named by its role (`no_role` for the seventh). The series holds the hours on the bar of every issue that finished that day, with a zero for an issue that has no hour on it. The five segments of the bar, and the count of the issues that hold hours on it, are counted beside. `metrics.FoldPhaseTime` merges a window's days. For each bar it answers the hours, the five segments, `issues` and `median_per_issue`. It also answers `idle_share`: the idle hours of every bar over the hours of every bar. The Legion section of the Delivery page reads this eval.

Each series has its roots cut (`<role>:roots`) and its children cut (`<role>:children`), as the series of legion-work-time have.

**What it does not count.** Every hour that the work time leaves out: parked hours, hours back in the queue, and hours before the first worker event. No hour of slot wait is in it.
