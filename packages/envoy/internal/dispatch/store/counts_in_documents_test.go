package store

import (
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/docnumbers"
)

// Every count the three documents of the connection-count check state of this package's pools,
// held to the constant that decides it: eleven constants in twenty-nine sentences, from the
// shared pool to the listeners, with the per-process total and the two-process ceiling beside
// them. Three rounds of dispatch://INFRA-558 went to one class: a constant is raised, every test
// of both sides stays green because no test reads a sentence, and the number a person reads is
// left behind. These entries read those sentences, so the next raise fails here instead of in a
// person's reading.
//
// The reader is internal/docnumbers, which internal/dispatch/delivery and
// internal/dispatch/processes register their own constants and documents with in the same shape;
// what is here is the registration, and nothing else. Each entry names one sentence: the file, a
// pattern over docnumbers.Number, the constant that number must equal, and what the number is of.
// A word reads as readily as digits, since the site page spells four of its counts and
// .dispatch/architecture/dispatch-store.md spells the lease pool's, and a pattern that matches
// nothing fails too.
//
// Two sentences of the site page are declined rather than registered, and this is where that is
// said. :404-405 break the listeners down - "Four of them wait on Postgres notifications" and
// "The fifth sends the cursors" - which are terms of listenerConnections rather than its total.
// The first is registered below as the total less one, since the reader writes cardinals. The second is not: "fifth"
// is an ordinal, which the reader cannot write, and a raise of listenerConnections is caught by
// the first term and by the three sentences that state the total, so the ordinal left alone
// costs that raise nothing.
//
// Keep it to the counts these three documents state of this package. A number another package
// owns belongs to that package's own registration (the heartbeat and the live window are
// internal/dispatch/processes'), and a number of something else - a migration version, a review
// round - belongs to no entry anywhere here.
func TestTheDocumentsStateTheCountsThisPackageHolds(t *testing.T) {
	const (
		site      = "docs/site/src/content/docs/dispatch/running-dispatch.md"
		readme    = "packages/envoy/cmd/dispatch/README.md"
		storeArch = ".dispatch/architecture/dispatch-store.md"
	)
	number := docnumbers.Number
	docnumbers.Check(t, filepath.Join("..", "..", "..", "..", ".."), []docnumbers.Stated{
		{File: site, Pattern: `up to ` + number + ` for the work it runs once across the servers`,
			Number: LeasePoolSize,
			Says:   "the connections one server opens for the work it runs once across the servers"},
		{File: site, Pattern: `That is\s+` + number + `\s+connections for one server at its ceiling`,
			Number: ConnectionsPerProcess, Says: "the connections one server opens at its ceiling"},
		{File: readme, Pattern: `callers of the two intakes' holds, ` + number,
			Number: LeasePoolSize,
			Says:   "the lease pool's connections, one per leased name and per hold caller"},
		{File: readme, Pattern: `= ` + number + ` connections to Postgres`,
			Number: ConnectionsPerProcess,
			Says:   "the sum of the counted pools and the permanent listeners"},
		{File: readme, Pattern: `opens at most 64 \+ ` + number + ` \+`, Number: LeasePoolSize,
			Says: "the lease term of that sum, which states the pool's own size inside it"},
		{File: readme, Pattern: `\| Leases \(` + "`store.LeasePoolSize`" + `\) \| ` + number + ` \|`,
			Number: LeasePoolSize, Says: "the lease pool's row of the pool table"},
		{File: readme, Pattern: `Two processes at their ceiling hold ` + number,
			Number: 2 * ConnectionsPerProcess, Says: "two processes at their ceiling"},
		{File: storeArch, Pattern: `per caller of a delivery intake's hold, ` + number,
			Number: LeasePoolSize,
			Says:   "the lease pool's connections, one per leased loop and per hold caller"},

		// The rest of the pool table, each row stating the constant beside its name, and the
		// site page's enumeration of the same pools in prose, where four of the counts are
		// spelled. One sentence each way for every pool, so a raise that moves one and not the
		// other fails here rather than in a person's reading.
		{File: readme, Pattern: `\| Shared \(` + "`sharedPoolSize`" + `\) \| ` + number + ` \|`,
			Number: sharedPoolSize, Says: "the shared pool's row of the pool table"},
		{File: site, Pattern: `up\s+to ` + number + ` connections for requests`,
			Number: sharedPoolSize, Says: "the connections one server opens for requests"},
		{File: readme, Pattern: `\| Room appends \(` + "`roomAppendsPoolSize`" + `\) \| ` + number + ` \|`,
			Number: roomAppendsPoolSize, Says: "the room-appends pool's row of the pool table"},
		{File: site, Pattern: `up to ` + number + ` for saving what people type in documents`,
			Number: roomAppendsPoolSize, Says: "the connections for saving what people type"},
		{File: readme, Pattern: `\| Rooms \(` + "`RoomsPoolSize`" + `\) \| ` + number + ` \|`,
			Number: RoomsPoolSize, Says: "the rooms pool's row of the pool table"},
		{File: site, Pattern: `up to ` + number + ` for loading\s+documents`,
			Number: RoomsPoolSize, Says: "the connections for loading documents"},
		{File: readme, Pattern: `\| Compaction \(` + "`compactionPoolSize`" + `\) \| ` + number + ` \|`,
			Number: compactionPoolSize, Says: "the compaction pool's row of the pool table"},
		{File: site, Pattern: `up to ` + number + ` for compacting documents`,
			Number: compactionPoolSize, Says: "the connections for compacting documents"},
		{File: readme, Pattern: `\| Health \(` + "`healthPoolSize`" + `\) \| ` + number + ` \|`,
			Number: healthPoolSize, Says: "the health pool's row of the pool table"},
		{File: site, Pattern: number + ` for the health check`,
			Number: healthPoolSize, Says: "the connection for the health check"},
		{File: readme, Pattern: `\| Outbox \(` + "`outboxPoolSize`" + `\) \| ` + number + ` \|`,
			Number: outboxPoolSize, Says: "the outbox pool's row of the pool table"},
		{File: site, Pattern: `for the health check, ` + number + ` for publishing`,
			Number: outboxPoolSize, Says: "the connections for publishing notifications"},
		{File: readme, Pattern: `\| Horizon \(` + "`horizonPoolSize`" + `\) \| ` + number + ` \|`,
			Number: horizonPoolSize, Says: "the horizon pool's row of the pool table"},
		{File: site, Pattern: `up to ` + number + ` for reading where the event log is safe`,
			Number: horizonPoolSize, Says: "the connections for reading the event horizon"},
		{File: readme, Pattern: `\| Registry \(` + "`registryPoolSize`" + `\) \| ` + number + ` \|`,
			Number: registryPoolSize, Says: "the registry pool's row of the pool table"},
		{File: site, Pattern: `and ` + number + ` for its entry in the list of running servers`,
			Number: registryPoolSize, Says: "the connection for its entry in the registry"},
		{File: readme, Pattern: `\| Listeners \(` + "`listenerConnections`" + `\) \| ` + number + ` \|`,
			Number: listenerConnections, Says: "the listeners' row of the pool table"},
		{File: readme, Pattern: `up to ` + number + ` Postgres connections per process outside the shared pool`,
			Number: listenerConnections, Says: "the listeners a process holds outside the shared pool"},
		{File: site, Pattern: `holds up to ` + number + ` more open for as long as it runs`,
			Number: listenerConnections, Says: "the listeners a server holds for as long as it runs"},
		// The breakdown of that total, one level down: the listeners that wait on a Postgres
		// notification are every one of them but the cursor sender, so this term is the total
		// less one and a raise leaves it false unless it is read here. The sentence beside it,
		// "The fifth sends the cursors", is declined in the header above.
		{File: site, Pattern: number + ` of them wait on Postgres notifications`,
			Number: listenerConnections - 1,
			Says:   "the listeners that wait on a Postgres notification, the total less the cursor sender"},
	})
}
