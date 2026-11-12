package processes

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/docnumbers"
)

// What the documents state of this package's own cadences, held to the variables that decide
// them. A reader of either document is told how often a process stamps its heartbeat and how
// recent a heartbeat must be for the process to count as live; both numbers are this package's,
// and until this row nothing held the sentence to the value.
//
// The reader is internal/docnumbers, which internal/dispatch/store and internal/dispatch/delivery
// register with too; this is data, one entry per sentence. Both cadences are durations, so each
// entry registers the number the sentence writes, which is the duration in seconds.
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
	})
}
