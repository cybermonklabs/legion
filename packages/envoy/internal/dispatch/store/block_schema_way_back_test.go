package store

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// productionSchemaVersion is the newest migration legion-envoy-v34.21.0 holds, the release
// Dispatch's production runs: 20261123080000_graph_supertags_plan_task_steps. Such a store has
// not run main's 20261125125701_graph_supertags_check_subject, so this release's boot applies it
// there, and what it applies under that version decides whether the releases that hold that
// migration still boot on the store afterwards.
const productionSchemaVersion = 20261123080000

// checkSubjectVersion is 20261125125701_graph_supertags_check_subject, which main shipped first
// in legion-envoy-v35.18.0 and every release through legion-envoy-v35.29.1 holds.
const checkSubjectVersion = 20261125125701

// mainCheckSubjectChecksum is the sha256 of the text main shipped as the file of
// checkSubjectVersion, which those releases hold. This branch leaves that file as main wrote it
// and writes the block schema's version again in a migration of its own above it
// (20261127042910), rather than editing main's file under a `-- replaces:` line. An edit would
// end the pre-deploy census's refusal in one deploy, and it would take the way back from every
// release of that band: a database this release created from empty would record a text none of
// them names, and pgmigrate.Reconcile refuses such a row, so that database would be reset rather
// than rolled back.
const mainCheckSubjectChecksum = "62571a5dda3b5be584c7ffc092a1403686b5d8a0cd047c34f1aa2dff80547c2d"

// blockSchemaVersionsOfThisBranch are the migrations of the block schema's version that this
// branch adds and no release of that band holds: 20261125012814 moves the schema to 11,
// 20261125034832 to 12, and 20261127042910 writes 12 again above main's checkSubjectVersion,
// which writes 6. A release that has no file of a version leaves the row as it is
// (pgmigrate.Reconcile), so these rows are the other half of the way back.
var blockSchemaVersionsOfThisBranch = []int{20261125012814, 20261125034832, 20261127042910}

// TestAStoreUpgradedFromProductionsSchemaStillBootsOnTheReleasesThatHoldMainsCheckSubject
// upgrades a store at productionSchemaVersion straight to this release, as the deploy of this
// release does to production, and then judges that store as the boot of a release of the
// legion-envoy-v35.18.0 to v35.29.1 band judges it (pgmigrate.Reconcile, which
// Store.prepareVersionTable runs at every boot): such a release must boot on it, so the deploy
// can be rolled back to the image that was running.
//
// It holds three things, each of which an edit of main's file would break:
//
//   - the file of checkSubjectVersion in this tree is the text main shipped
//     (mainCheckSubjectChecksum), so the band's file of that version is this one;
//   - the store records the three versions of blockSchemaVersionsOfThisBranch, which that band
//     has no file of, so the judgment meets them and leaves them as they are;
//   - that judgment refuses nothing.
//
// The control injects the fault an edit would cause: the same release, the same store, with the
// row of checkSubjectVersion recording an applied text the band's file does not name. The
// judgment must refuse it and name the version. Without that control a judgment that refuses
// nothing says only that it was asked nothing.
func TestAStoreUpgradedFromProductionsSchemaStillBootsOnTheReleasesThatHoldMainsCheckSubject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, productionSchemaVersion)

	band := releasesThatHoldMainsCheckSubject(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("upgrade a store at %d to this release: %v", productionSchemaVersion, err)
	}
	recorded, err := pgmigrate.ReadRecorded(ctx, store.Pool, versionTable)
	if err != nil {
		t.Fatalf("read what %s records after the upgrade: %v", versionTable, err)
	}

	for _, version := range blockSchemaVersionsOfThisBranch {
		if !slices.ContainsFunc(recorded, func(row pgmigrate.Recorded) bool { return row.Version == version }) {
			t.Fatalf("%s records no row of %d after the upgrade, so the judgment below never meets a version the band has no file of", versionTable, version)
		}
	}
	if _, err := pgmigrate.Reconcile(band, recorded); err != nil {
		t.Errorf("a release of the legion-envoy-v35.18.0 to v35.29.1 band refuses a store this release upgraded from %d: %v", productionSchemaVersion, err)
	}

	asIfEdited := slices.Clone(recorded)
	at := slices.IndexFunc(asIfEdited, func(row pgmigrate.Recorded) bool { return row.Version == checkSubjectVersion })
	if at < 0 {
		t.Fatalf("%s records no row of %d after the upgrade, so the control cannot inject the fault an edit of that file would cause", versionTable, checkSubjectVersion)
	}
	asIfEdited[at].Applied = pgmigrate.Checksum([]byte("the text an edit of main's file would have applied"))
	_, err = pgmigrate.Reconcile(band, asIfEdited)
	switch {
	case err == nil:
		t.Errorf("control: a release of the band accepts a store recording an applied text of %d that its file does not name, so this judgment cannot see the cost an edit of that file would carry", checkSubjectVersion)
	case !pgmigrate.IsRefusal(err):
		t.Errorf("control: the judgment failed rather than refusing the row of %d: %v", checkSubjectVersion, err)
	case !strings.Contains(err.Error(), strconv.Itoa(checkSubjectVersion)):
		t.Errorf("control: the refusal does not name version %d: %v", checkSubjectVersion, err)
	}
}

// releasesThatHoldMainsCheckSubject is the set of migrations a release of the
// legion-envoy-v35.18.0 to v35.29.1 band holds, as far as a judgment of this store's rows reads
// it: this tree's set without the three versions of blockSchemaVersionsOfThisBranch, which no
// such release holds. Every other file of this tree is a file that band holds with the same text,
// or one it has no row of, and either way the judgment reads it as that release's boot does. It
// fails unless the file of checkSubjectVersion is still the text main shipped
// (mainCheckSubjectChecksum): against any other text the set is not that band's, and the test
// would prove nothing about those releases.
func releasesThatHoldMainsCheckSubject(t *testing.T) []pgmigrate.Migration {
	t.Helper()
	var band []pgmigrate.Migration
	held := false
	for _, migration := range shippedMigrations(t) {
		if slices.Contains(blockSchemaVersionsOfThisBranch, migration.Version) {
			continue
		}
		if migration.Version == checkSubjectVersion {
			if migration.Checksum != mainCheckSubjectChecksum {
				t.Fatalf("%s is sha256 %s, and the releases from legion-envoy-v35.18.0 hold sha256 %s: this tree edits a migration those releases applied, which they refuse unless the file names what they record in a `-- replaces:` line",
					migration.Name, migration.Checksum, mainCheckSubjectChecksum)
			}
			held = true
		}
		band = append(band, migration)
	}
	if !held {
		t.Fatalf("this tree has no migration of version %d, which every release of the band holds", checkSubjectVersion)
	}
	return band
}
