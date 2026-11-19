//go:build twobuild

// The migration half of the two-build suite: a release boots over a database the next release's
// runner has booted, as a rolling update and the auto-deployer's version-only rollback need, and as
// this lane needs once a release that judges the applied text is the previous build
// (cmd/dispatch/README.md, "A recorded version under another file").

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// editedMigration is the shipped migration the next release edits in place.
const editedMigration = "0001_init.up.sql"

// nextRelease is release N+1: this tree, N, with editedMigration edited in place as a pull request
// edits a shipped migration, declaring N's identity of the file in the replaces line the pre-merge
// check requires (pgmigrate.CheckEdits). It is built with go build -overlay, so the tree is not
// changed. It answers the binary, and N's text of the file and N+1's.
func nextRelease(t *testing.T) (binary string, original, edited []byte) {
	t.Helper()
	source, err := filepath.Abs(filepath.Join("..", "..", "internal", "dispatch", "store", "migrations", editedMigration))
	if err != nil {
		t.Fatal(err)
	}
	original, err = os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", editedMigration, err)
	}
	edited = append(append([]byte{}, original...), []byte("\n-- an edit of release N+1\n-- replaces: "+editedMigration+" sha256:"+pgmigrate.Checksum(original)+"\n")...)
	dir := t.TempDir()
	replacement := filepath.Join(dir, editedMigration)
	if err := os.WriteFile(replacement, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {source: replacement}})
	if err != nil {
		t.Fatal(err)
	}
	overlayFile := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlayFile, overlay, 0o644); err != nil {
		t.Fatal(err)
	}
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: "envoy-dispatch-next", Flags: []string{"-overlay", overlayFile}}), original, edited
}

// refusedBoot starts binary on the database url names and requires it to refuse to start: exit 1,
// with want in its output.
func refusedBoot(t *testing.T, b *twoBuilds, url, binary, which, want string) {
	t.Helper()
	options := b.options(nil)
	options.Binary = binary
	refused := startDispatchProcessWith(t, url, options)
	refused.WaitExit(t, which)
	output := refused.Output.String()
	if code := refused.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("%s exited %d; want 1, a refusal", which, code)
	}
	if !strings.Contains(output, want) {
		t.Errorf("%s: the output does not say %q:\n%s", which, want, output)
	}
}

// Release N is this tree, and N+1 the next release (nextRelease). The row's database is the lane's
// (laneDatabase): the previous build migrated it from empty and this tree migrated it forward.
// N+1 boots over it and accepts the row of the edited version as it is, so N, and the release
// before this change, boot over it again. The control writes the row N+1's runner would have
// written had it moved the row to the file's own identity, the earlier design: N then refuses the
// row and exits 1, naming it, which is the boot this row exists to keep.
func TestTheReleaseBeforeBootsOverADatabaseTheNextReleaseBootedWithAReplacedFile(t *testing.T) {
	b := newTwoBuilds(t)
	ctx := context.Background()
	next, original, edited := nextRelease(t)
	// recorded reads what version 1's row records: its identity, and the text applied, which the
	// lane's database records as N's text, or as none where this tree filled the row the previous
	// build's runner wrote with no name.
	recorded := func() (name, checksum, applied string) {
		t.Helper()
		var text *string
		if err := b.database.Pool.QueryRow(ctx, "select name, checksum, applied_checksum from schema_migrations where version = 1").Scan(&name, &checksum, &text); err != nil {
			t.Fatalf("read the row of version 1: %v", err)
		}
		if text != nil {
			applied = *text
		}
		return name, checksum, applied
	}
	boot := func(binary, which string) {
		t.Helper()
		options := b.options(nil)
		options.Binary = binary
		p := startDispatchProcessWith(t, b.databaseURL(t), options)
		p.waitHealthy(t)
		stop(t, p)
		t.Logf("%s booted and stopped", which)
	}
	name, checksum, applied := recorded()
	if name != editedMigration || checksum != pgmigrate.Checksum(original) || (applied != "" && applied != pgmigrate.Checksum(original)) {
		t.Fatalf("version 1 records %s sha256:%s, applied %q, before N+1 boots; want N's own %s sha256:%s and N's text or none", name, checksum, applied, editedMigration, pgmigrate.Checksum(original))
	}
	boot(next, "release N+1, whose file declares N's identity,")
	if n, c, a := recorded(); n != name || c != checksum || a != applied {
		t.Errorf("after N+1 booted, version 1 records %s sha256:%s, applied %q; want the row left as it was", n, c, a)
	}
	boot(b.binary, "release N, this tree,")
	options := b.options(nil)
	options.Binary, options.WebDist = b.build.path("envoy-dispatch"), b.build.path("web")
	previous := startDispatchProcessWith(t, b.databaseURL(t), options)
	previous.waitHealthy(t)
	stop(t, previous)
	t.Logf("the %s booted and stopped", b.previousName())

	// Control: the row as the earlier design moved it.
	if _, err := b.database.Pool.Exec(ctx, "update schema_migrations set checksum = $1 where version = 1", pgmigrate.Checksum(edited)); err != nil {
		t.Fatalf("control: move the row to N+1's identity: %v", err)
	}
	refusedBoot(t, b, b.databaseURL(t), b.binary, "control: release N over the moved row",
		"migrations refused, none applied: version 1 is recorded as "+editedMigration+" sha256:"+pgmigrate.Checksum(edited))
}

// Once this runner is the previous build, this lane boots it over a database a pull request's tree
// migrated. A pull request that edits a shipped migration in place, with the replaces line the
// pre-merge check requires, must keep the lane green. Here this tree, N, stands for that previous
// build, since its runner judges the applied text, and N+1 (nextRelease) for the pull request's
// tree. The lane's database, built in production's order (migrateInOrder: N migrates it from
// empty, N+1 migrates it forward), records N's text of the edited version as applied, and N boots
// over it. The control is the lane's old setup, a database N+1 migrated from empty, which records
// N+1's text as applied: N refuses it and exits 1, naming that text, which is the red lane
// production's order keeps away.
func TestTheReleaseBeforeBootsOverTheLaneDatabaseOfAPullRequestThatEditsAShippedMigration(t *testing.T) {
	b := newTwoBuilds(t)
	next, original, edited := nextRelease(t)

	lane := storetest.Empty(t)
	migrateInOrder(t, lane, b.binary, next)
	options := b.options(nil)
	options.Binary = b.binary
	release := startDispatchProcessWith(t, lane, options)
	release.waitHealthy(t)
	stop(t, release)
	t.Logf("release N booted over the lane's database, which N migrated from empty and N+1 migrated forward")

	old := storetest.Empty(t)
	migrateInOrder(t, old, next)
	refusedBoot(t, b, old, b.binary, "control: release N over a database N+1 migrated from empty",
		"version 1 is recorded as "+editedMigration+" sha256:"+pgmigrate.Checksum(original)+", which this build's file of version 1, "+editedMigration+" sha256:"+pgmigrate.Checksum(original)+", accepts, but the text applied under it has sha256 "+pgmigrate.Checksum(edited))
}
