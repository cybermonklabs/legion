package processes

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/docnumbers"
)

// What the documents state of this package's own cadences, held to the variables that decide
// them: how often a process stamps its heartbeat, how recent a heartbeat must be for the process
// to count as live, and the one sentence that states their sum. Both are durations, so each entry
// registers the number the sentence writes, which is the duration in seconds. The class and the
// reader are internal/docnumbers' own documentation; what is here is the registration.
//
// Two sentences of these documents state a cadence this package does not own, and they are
// declined here rather than left silent, which is what a census of unread numbers would
// otherwise find. packages/envoy/cmd/dispatch/README.md:841 states the delivery hold's lease and
// its renewal, "a row with a 15-second lease on the database's clock, renewed every five
// seconds", which are internal/dispatch/duty's; and the site page's line 362 states the event
// stream's heartbeat, 15 seconds, which is internal/dispatch/api's defaultStreamHeartbeat. This
// registration covers the constants its own package owns, and whichever package owns those two
// registers them with the same reader when it chooses.
func TestTheDocumentsStateTheCadencesThisPackageHolds(t *testing.T) {
	const (
		site   = "docs/site/src/content/docs/dispatch/running-dispatch.md"
		readme = "packages/envoy/cmd/dispatch/README.md"
	)
	number := docnumbers.Number
	heartbeat, live := int(HeartbeatInterval/time.Second), int(LiveWindow/time.Second)
	docnumbers.Check(t, filepath.Join("..", "..", "..", "..", ".."), []docnumbers.Stated{
		{File: site, Pattern: `which it renews every ` + number + ` seconds`, Number: heartbeat,
			Says: "how often a server renews its entry in the list of running servers"},
		{File: readme, Pattern: `replaces those ` + number + ` seconds for an end-to-end test`,
			Number: heartbeat, Says: "the heartbeat a test setting replaces"},
		{File: site, Pattern: `stopped reporting in for longer than ` + number + ` seconds`,
			Number: live, Says: "how long a server may go unheard of before it stops counting as live"},
		{File: readme, Pattern: `one live window \(` + number + ` seconds\)`, Number: live,
			Says: "the live window a process waits out before it takes a refused name"},

		// Every other sentence of these two documents that states one of the two cadences,
		// enumerated rather than sampled: the reviewer's round 30 found four read of at least
		// ten, and a guard that reads some of the places its number is stated leaves the class
		// alive where it does not look.
		{File: site, Pattern: `waits up to ` + number + ` seconds and one more\s+report`,
			Number: live, Says: "the live window a starting server waits out before it exits"},
		{File: site, Pattern: `one more\s+report \(` + number + ` seconds\)`, Number: heartbeat,
			Says: "the one further report that server waits for"},
		{File: readme, Pattern: `heartbeating every ` + number + ` seconds`, Number: heartbeat,
			Says: "how often a live process heartbeats while it refuses a restart"},
		{File: readme, Pattern: `stamps its ` + "`server_processes`" + ` row every ` + number + ` seconds`,
			Number: heartbeat, Says: "how often a process stamps its registry row"},
		{File: readme, Pattern: `counts as live while that heartbeat is within ` + number + ` seconds`,
			Number: live, Says: "how recent a heartbeat must be for its process to count as live"},
		{File: readme, Pattern: `counts as live until ` + number + ` seconds after its last heartbeat`,
			Number: live, Says: "how long a left-behind row counts as live"},
		// The one sentence that states neither cadence but their sum: a restart after a kill
		// waits out the live window and the heartbeat that ends it, so the number a reader is
		// given moves when either constant moves, and this entry is what says so.
		{File: readme, Pattern: `waits up to about ` + number + ` seconds for that row to lapse`,
			Number: live + heartbeat,
			Says:   "the wait a restart after a kill takes, the live window and one heartbeat"},

		// The four sentences a subject pass over the registry, the heartbeat and the live window
		// found unread: a census that flags a line only where it names its constant in backticks
		// is a lower bound, and these four name neither constant.
		{File: readme, Pattern: `live by the registry's ` + number + `-second window`, Number: live,
			Says: "the window by which a CI row's process counts as live"},
		{File: readme, Pattern: `by its heartbeat, for ` + number + ` seconds after the last one`,
			Number: live, Says: "how long the registry counts a process after its last heartbeat"},
		{File: readme, Pattern: `registry of live server processes every ` + number + ` seconds`,
			Number: heartbeat, Says: "how often the registry pool's connection stamps its row"},
		{File: readme, Pattern: `Every ` + number + ` seconds \(the registry's live window\)`,
			Number: live, Says: "how often a process re-checks its open document sockets"},
	})
}
