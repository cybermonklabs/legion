package api

// 20261125181543 look-names-its-check: a production check's look report names its check:
// HandoffCompleteRequest gains optional checkOpenedAt, which alone selects the check route.
// Without it a call completes a phase, regardless of the issue's phase, an open check or the task
// a claim is serving. The route refuses a different or absent open check, another role, then an
// unserved look, before any fact. It refuses commit, handoffId or handoffVersion on the check
// path with the 400 CHECK_PHASE_FIELDS_UNEXPECTED. A served look survives phase tasks until the
// next look. A late phase completion needs this run's EntryTask and its fresh handoff. The tool's
// check_opened_at carries the look task's RFC3339 time with stored microseconds. Both plugins
// leave phase input open after a report.
// An earlier daemon rejects the new member and an earlier plugin sends none, so the boot and
// image gates require a paired daemon, plugin and worker image. This request-only change adds
// no answer key or enumeration value: no shaping band changes, and every previous contract's
// answer and request record stays intact. Before rollback, clear kept looks only for running
// issues of the rolled-back project, as docs/kubernetes.md documents and the store test
// exercises.
const lookNamesItsCheckContract = 20261125181543

func init() {
	registerEntry(contractEntry{id: lookNamesItsCheckContract, slug: "look-names-its-check"})
}
