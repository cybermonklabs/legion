package textcheck

// This test reads the look id's migration through migrations.WayBack, and internal/store's
// look_id_way_back_test.go and migration_20261127171352_test.go, which run that migration's way
// back, read no text outside their package, so the guide is this one's one row.
//ci:reads docs/kubernetes.md wayBackBlockOfTheGuide reads it under ../../../.. (shippedText) for TestTheLookIDWayBackIsOneTextInTheHeaderAndTheGuide

import "testing"

// The way back of the release whose look report gives its look id stands in two files: the header
// of store migration 20261127171352_production_check_look_id, which ships the block with the
// migration, and a fenced block of docs/kubernetes.md, under "Rolling back the release whose look
// report gives its look id", which is where an operator reads it at a rollback and which the header
// names as the release's whole way back. internal/store's
// TestTheWayBackStripsTheLookIDRowsAndClosesTheChecksOfChildrenThatRunAgain and
// TestTheLookIDColumnsArriveEmptyAndTheWayBackStripsWhatTheReleaseBeforeRefuses run the header's
// block over a store and read what it leaves; this test holds the guide's block to that one, so the
// block an operator runs is the block those tests prove. The split keeps the guide out of the
// daemon Go lane's filter, as the phase entry's way back does (way_back_doc_test.go).
//
// The header's block is read by the one reader of a migration's way back (migrations.WayBack),
// which internal/store's tests run, so the text held to the guide is the text those tests run, and
// the guide's block is the one under that heading that holds the one line only that rollback's
// block holds (wayBackBlockOfTheGuide).
const (
	// lookIDVersion is the version of the migration whose header carries the block, which
	// migrations.WayBack reads it by and whose name the embedded set gives (migrationNames), so
	// the header this test reads and the entry it holds the guide to are one migration.
	lookIDVersion = 20261127171352
	// lookIDWayBackSection is the heading of the guide's section that gives the way back.
	lookIDWayBackSection = "Rolling back the release whose look report gives its look id"
	// lookIDWayBackMark is the line that deletes the retire_look rows, which no other rollback's
	// block carries.
	lookIDWayBackMark = "delete from outbox where kind = 'supervise' and payload->>'op' = 'retire_look'"
)

// The guide's way back of the release whose look report gives its look id is the block the
// migration's header carries, line for line. An edit to either side alone fails here: a line
// changed in the guide and not in the header would otherwise never reach the tests that run the
// header's block.
func TestTheLookIDWayBackIsOneTextInTheHeaderAndTheGuide(t *testing.T) {
	name := "the header of " + migrationNames(t, lookIDVersion)[0]
	assertIsTheGuidesBlock(t, heldBlock{Text: headersWayBack(t, lookIDVersion, name), Name: name, Label: "the header",
		Why: "internal/store's tests run the header's block and an operator the guide's"},
		lookIDWayBackSection, lookIDWayBackMark)
}
