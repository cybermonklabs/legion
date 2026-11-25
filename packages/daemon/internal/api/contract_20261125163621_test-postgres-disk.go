package api

// 20261125163621 test-postgres-disk: dispatch://INFRA-1117 -- the test database's own disk. Where
// `runtime.kubernetes.test_postgres_disk` sets a size, an issue pod that runs the test database
// keeps its cluster on a generic ephemeral volume of that size in place of the `testpg` emptyDir,
// and the launcher's command line gains `--test-postgres-disk` after `--test-postgres`
// (sandbox's issuePod.launcherContainer), which tells the launcher that the free bytes it reads
// there are the cluster's own, so it resets the cluster under a floor (launcher's testpg.go). A
// pod whose key is off gets no such flag, and its command line is unchanged. The state's settings
// block carries the key as `settings.runtime.kubernetes.test_postgres_disk`, absent while it is
// off, which the plugin parses loosely (version.go's entry 35), so that key alone would add no
// entry.
//
// The skew is safe in both directions only through this entry. An older launcher refuses an
// unknown flag and exits, so a daemon of this release must never render `--test-postgres-disk`
// into a pod whose image predates it: the image probe pairs a daemon with an image by their
// daemon API contract (`runtime/sandbox/probe.go`), as it did for entries 137 and 151, so a daemon
// of this release is paired only with an image of this release. An older daemon renders no such
// flag, and a launcher of this release given none behaves as before: it reads no free bytes and
// resets nothing. No frame between the daemon and the launcher moves, and no shape the plugin
// parses.
const testPostgresDiskContract = 20261125163621

func init() {
	registerEntry(contractEntry{id: testPostgresDiskContract, slug: "test-postgres-disk"})
}
