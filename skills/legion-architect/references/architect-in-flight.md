# Children and work in flight

Read this before you set a child's `--blocked-by` links or park or rerun a child, on each
`child-closed`, and while the work is built (`skill://legion-architect`, sections 2 to 4).

## Links, parks and reruns

- Set a child's dependencies in one call: `dispatch issue-update --issue <child> --blocked-by <a>
  --blocked-by <b>`. It replaces the whole list, so read the existing links first (`dispatch read`).
- On each `child-closed`, read the child's handoff, edit the specs of open children it changes,
  and `park_child` each child it makes unneeded, and say why with `dispatch comment`.
- Park a child only when a person asked for that move, or when the work is done, folded into
  another issue or no longer needed, and say with `dispatch comment` which of those it is.
- Before you park a child for good, take it off each sibling's `--blocked-by` list
  (`--clear-blocked-by` where it was the only one), or park that sibling too: a parked child is
  not `done`, and its dependents stay held with no notice.
- A park followed by `rerun_child` keeps its dependents' links: they wait through the rerun and
  start when the rerun's merge closes the child.
- A child whose work has not ended is not parked to let its tree reach the end: you ask the person
  whether its work must end with the root (a child's architect asks through the architect above
  it, as "Every turn" in `skill://legion-architect` says), and the root's close hands such a child
  to Legion as a root of its own (`skill://legion-architect/references/acceptance-close.md`,
  "Hand-over at a root's linger").

## Who holds a child

- A child with no `architect` entry in its record (`read_record`) has no architect of its own yet.
  The architect above it keeps it until the child's next first phase start gives it one.
- A released child takes an admission slot of its own when the daemon starts its first phase, and
  it waits in the admission line for one. It gives that slot back while it waits on another issue,
  a `blocked_by` link to an issue that is not done or an open child its own first phase waits for,
  and nothing of its work stops; past its review it holds none. It never becomes a root while your
  tree is live.

## While the work is built

- One role holds the issue's phase at a time, and every role that has a session may work beside it
  and tell the role whose phase it is what it finds: for example, the reviewer reads each push as
  it lands, and the tester tries an intermediate build. You can ask them to. A role has no session
  before its first assignment, so in an issue's first round the reviewer and the tester do not
  exist yet.
- While the work is built, the tester runs the application from a build of the issue's branch and
  uses it as its user would: a passing check does not show that a person can do their job with it.
  Read the tester's handoff for that use.
- Where the work is a page or a tool a person uses, offer that person a running build before the
  review; the deployment instructions say how a pod runs one.
