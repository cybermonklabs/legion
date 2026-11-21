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
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// editedMigration is the shipped migration the next release edits in place. attributionRule is
// the one #4466 edited in place, a comment scrub, before the runners judged identities: the lane
// row edits it again, as the next such pull request would.
const (
	editedMigration = "0001_init.up.sql"
	attributionRule = "20261105070440_pull_request_heads_attribution_rule.up.sql"
)

// editedFile is one migration nextRelease edits: N's file as N's runner loads it (pgmigrate.Load),
// and N+1's text of it.
type editedFile struct {
	release pgmigrate.Migration
	edited  []byte
}

// nextRelease is release N+1: this tree, N, with each of names edited in place as a pull request
// edits a shipped migration, declaring N's identity of the file in the replaces line the pre-merge
// check requires (pgmigrate.CheckEdits). It is built with go build -overlay, so the tree is not
// changed. It answers the binary and, in the order of names, each file as N loads it with N+1's
// text of it.
func nextRelease(t *testing.T, names ...string) (string, []editedFile) {
	t.Helper()
	dir := t.TempDir()
	replace := map[string]string{}
	set, err := pgmigrate.Load(os.DirFS(filepath.Join("..", "..", "internal", "dispatch", "store")), "migrations")
	if err != nil {
		t.Fatalf("load this tree's migrations: %v", err)
	}
	var files []editedFile
	for _, name := range names {
		index := slices.IndexFunc(set, func(m pgmigrate.Migration) bool { return m.Name == name })
		if index < 0 {
			t.Fatalf("this tree has no migration %s", name)
		}
		release := set[index]
		source, err := filepath.Abs(filepath.Join("..", "..", "internal", "dispatch", "store", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		edited := []byte(release.SQL + "\n-- an edit of release N+1\n-- replaces: " + release.Identity().String() + "\n")
		replacement := filepath.Join(dir, name)
		if err := os.WriteFile(replacement, edited, 0o644); err != nil {
			t.Fatal(err)
		}
		replace[source] = replacement
		files = append(files, editedFile{release: release, edited: edited})
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": replace})
	if err != nil {
		t.Fatal(err)
	}
	overlayFile := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlayFile, overlay, 0o644); err != nil {
		t.Fatal(err)
	}
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: "envoy-dispatch-next", Flags: []string{"-overlay", overlayFile}}), files
}

// accepts says whether m accepts the applied text whose sha256 is checksum: its own text, or one a
// replaces line of it names.
func accepts(m pgmigrate.Migration, checksum string) bool {
	return checksum == m.Checksum || slices.ContainsFunc(m.Replaces, func(replaced pgmigrate.Identity) bool { return replaced.Checksum == checksum })
}

// appliedTextRefusal is how m's runner refuses a row of m's version that records m's first identity
// (Migration.RecordedIdentity) and the applied text whose sha256 is applied, a text m does not
// accept: the sentence names the row, m's own identity and each replaces line m has.
func appliedTextRefusal(m pgmigrate.Migration, applied string) string {
	declared := ""
	if len(m.Replaces) > 0 {
		names := make([]string, len(m.Replaces))
		for i, replaced := range m.Replaces {
			names[i] = replaced.String()
		}
		declared = ", whose replaces lines name only " + strings.Join(names, ", ")
	}
	version := strconv.Itoa(m.Version)
	return "version " + version + " is recorded as " + m.RecordedIdentity().String() + ", which this build's file of version " + version + ", " + m.Identity().String() + declared + ", accepts, but the text applied under it has sha256 " + applied
}

// refusedBoot starts binary on the database url names and requires it to refuse to start: exit 1,
// with want in its output.
func refusedBoot(t *testing.T, b *twoBuilds, url, binary, which, want string) {
	t.Helper()
	options := b.options(nil, nil)
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
	next, files := nextRelease(t, editedMigration)
	edited, release := files[0].edited, files[0].release
	// recorded reads what version 1's row records: the identity N's runner records of its file, and
	// the text applied, which the lane's database records as a text N's file accepts, or as none
	// where this tree filled the row the previous build's runner wrote with no name.
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
		options := b.options(nil, nil)
		options.Binary = binary
		p := startDispatchProcessWith(t, b.databaseURL(t), options)
		p.waitHealthy(t)
		stop(t, p)
		t.Logf("%s booted and stopped", which)
	}
	name, checksum, applied := recorded()
	first := release.RecordedIdentity()
	if name != first.Name || checksum != first.Checksum || (applied != "" && !accepts(release, applied)) {
		t.Fatalf("version 1 records %s sha256:%s, applied %q, before N+1 boots; want %s, the identity N's runner records of its file, and a text N's file accepts, or none", name, checksum, applied, first)
	}
	boot(next, "release N+1, whose file declares N's identity,")
	if n, c, a := recorded(); n != name || c != checksum || a != applied {
		t.Errorf("after N+1 booted, version 1 records %s sha256:%s, applied %q; want the row left as it was", n, c, a)
	}
	boot(b.binary, "release N, this tree,")
	options := b.options(nil, nil)
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
		"migrations refused, none applied: version 1 is recorded as "+first.Name+" sha256:"+pgmigrate.Checksum(edited))
}

// Once this runner is the previous build, this lane boots it over a database a pull request's tree
// migrated. A pull request that edits a shipped migration in place, with the replaces line the
// pre-merge check requires, must keep the lane green. Here this tree, N, stands for that previous
// build, since its runner judges the applied text, and N+1 (nextRelease) for the pull request's
// tree, which edits 0001_init and #4466's migration. The row's database comes from laneDatabase,
// where every row's database comes from, given N as the earlier build and N+1 as the later: N
// migrates it from empty and N+1 migrates it forward, so it records N's text of each edited
// version as applied, and N boots over it. Were laneDatabase to put the later build first, N would
// refuse that database as the template is made, and this row would fail. The control is the lane's
// old setup, a database N+1 migrated from empty, which records N+1's texts as applied: N refuses it
// and exits 1, naming each, which is the red lane production's order keeps away.
func TestTheReleaseBeforeBootsOverTheLaneDatabaseOfAPullRequestThatEditsAShippedMigration(t *testing.T) {
	b := newTwoBuilds(t)
	next, files := nextRelease(t, editedMigration, attributionRule)

	lane := laneDatabase(t, "release N, then N+1 editing "+editedMigration+" and "+attributionRule, b.binary, next)
	options := b.options(nil, nil)
	options.Binary = b.binary
	release := startDispatchProcessWith(t, databaseURLOf(t, lane), options)
	release.waitHealthy(t)
	stop(t, release)
	t.Logf("release N booted over the lane's database (laneDatabase), which N migrated from empty and N+1, editing %s and %s, migrated forward", editedMigration, attributionRule)

	old := storetest.Empty(t)
	migrateInOrder(t, old, next)
	// One boot: its refusal names every refused version, so each file's line is read from it.
	options = b.options(nil, nil)
	options.Binary = b.binary
	refused := startDispatchProcessWith(t, old, options)
	refused.WaitExit(t, "control: release N over a database N+1 migrated from empty")
	if code := refused.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("control: release N exited %d over a database N+1 migrated from empty; want 1, a refusal", code)
	}
	output := refused.Output.String()
	for _, file := range files {
		if want := appliedTextRefusal(file.release, pgmigrate.Checksum(file.edited)); !strings.Contains(output, want) {
			t.Errorf("control: release N's refusal does not say %q:\n%s", want, output)
		}
	}
}
