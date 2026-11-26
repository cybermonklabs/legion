**How it is measured.** The eval counts the project's issues that finished in the window, as legion-work-time counts them. It shares the work time of each issue out over seven bars, one for each worker type, by the issue's phase:

- architect: `design` and `accepting`
- planner: `planning`
- implementer: `implementing` and `retro`
- tester: `testing`
- reviewer: `reviewing`
- merger: `merging` and `awaiting_merge`
- no role: `awaiting_person` and `held`

The hours in the queue and the hours out of the workflow before the close, as legion-work-time counts them, are on no bar. So the seven bars of an issue add up to its work time, and this eval and legion-work-time agree. When the daemon stops reporting an issue, its sampler writes `null` for every field of the issue at one moment, and that is a drop, not a release. The phase and the slot that the issue held run on through a drop until the next observation that names a phase, and from that moment the issue holds a slot only if a slot observation at the same moment names one; where no such observation comes, they run to the issue's end.

The hours of each bar split into five segments. Each hour goes to the first segment of this list that holds through it:

1. Waiting on a person: the daemon recorded the root in `awaiting_person`, or an ask that a session of the tree opened on the issue was on the person's turn.
2. In a turn: a role of the issue had the claim state `working`. A claim that a drop writes `null` is no claim.
3. CI wait: not measured. No source says when a pull request waits on CI since the daemon stopped carrying its CI verdict, so an hour it would have held goes to the next segment that holds, and the segment stays at zero in its place.
4. Waiting on children: the issue is a root with a child in a phase from planning to awaiting merge, during that child's own work time. A child out of the workflow is in none of those phases and outside its work time, so the root waits on it through none of those hours.
5. Idle: every hour that no segment above claimed.

`legionfacts` stores the part of each bar that each issue holds. For each UTC day, `metrics.PhaseTimeDay` builds one series for each bar, named by its role (`no_role` for the seventh). The series holds the hours on the bar of every issue that finished that day, with a zero for an issue that has no hour on it. The five segments of the bar, and the count of the issues that hold hours on it, are counted beside. `metrics.FoldPhaseTime` merges a window's days. For each bar it answers the hours, the five segments, `issues` and `median_per_issue`. It also answers `idle_share`: the idle hours of every bar over the hours of every bar.

Each series has its roots cut (`<role>:roots`) and its children cut (`<role>:children`), as the series of legion-work-time have.

**What it does not count.** Every hour that the work time leaves out: parked hours, hours back in the queue, hours out of the workflow before the close, and hours before the first worker event. No hour of slot wait is in it.
