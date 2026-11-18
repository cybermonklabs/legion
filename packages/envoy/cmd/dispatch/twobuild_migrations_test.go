//go:build twobuild

// The migration half of the two-build suite: a release boots over a database the next release's
// runner has booted, as a rolling update and the auto-deployer's version-only rollback need
// (cmd/dispatch/README.md, "A recorded version under another file").

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// editedMigration is the shipped migration the next release edits in place.
const editedMigration = "0001_init.up.sql"

// sha256Hex is the checksum a runner records of text.
func sha256Hex(text []byte) string {
	sum := sha256.Sum256(text)
	return hex.EncodeToString(sum[:])
}

// Release N is this tree. Release N+1 is this tree with editedMigration edited in place, declaring
// N's identity of the file in a replaces line; it is built with go build -overlay, so the tree is
// not changed. N+1 boots over a database N's runner migrated and accepts N's row as it is, so N,
// and the release before this change, boot over that database again. The control writes the row
// N+1's runner would have written had it moved the row to the file's own identity, the earlier
// design: N then refuses the row and exits 1, naming it, which is the boot this row exists to keep.
func TestTheReleaseBeforeBootsOverADatabaseTheNextReleaseBootedWithAReplacedFile(t *testing.T) {
	b := newTwoBuilds(t)
	ctx := context.Background()
	source, err := filepath.Abs(filepath.Join("..", "..", "internal", "dispatch", "store", "migrations", editedMigration))
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", editedMigration, err)
	}
	edited := append(append([]byte{}, original...), []byte("\n-- an edit of release N+1\n-- replaces: "+editedMigration+" sha256:"+sha256Hex(original)+"\n")...)
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
	next := cmdtest.BuildCommand(t, cmdtest.Command{Name: "envoy-dispatch-next", Flags: []string{"-overlay", overlayFile}})
	recorded := func() (name, checksum string) {
		t.Helper()
		if err := b.database.Pool.QueryRow(ctx, "select name, checksum from schema_migrations where version = 1").Scan(&name, &checksum); err != nil {
			t.Fatalf("read the row of version 1: %v", err)
		}
		return name, checksum
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
	if name, checksum := recorded(); name != editedMigration || checksum != sha256Hex(original) {
		t.Fatalf("version 1 records %s sha256:%s before N+1 boots; want N's own %s sha256:%s", name, checksum, editedMigration, sha256Hex(original))
	}
	boot(next, "release N+1, whose file declares N's identity,")
	if name, checksum := recorded(); name != editedMigration || checksum != sha256Hex(original) {
		t.Errorf("after N+1 booted, version 1 records %s sha256:%s; want N's %s sha256:%s left as it was", name, checksum, editedMigration, sha256Hex(original))
	}
	boot(b.binary, "release N, this tree,")
	options := b.options(nil)
	options.Binary, options.WebDist = b.build.path("envoy-dispatch"), b.build.path("web")
	previous := startDispatchProcessWith(t, b.databaseURL(t), options)
	previous.waitHealthy(t)
	stop(t, previous)
	t.Logf("the %s booted and stopped", b.previousName())

	// Control: the row as the earlier design moved it.
	if _, err := b.database.Pool.Exec(ctx, "update schema_migrations set checksum = $1 where version = 1", sha256Hex(edited)); err != nil {
		t.Fatalf("control: move the row to N+1's identity: %v", err)
	}
	options = b.options(nil)
	options.Binary = b.binary
	refused := startDispatchProcessWith(t, b.databaseURL(t), options)
	refused.WaitExit(t, "a boot over the moved row")
	output := refused.Output.String()
	if code := refused.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("control: release N exited %d over the moved row; want 1", code)
	}
	want := "migrations refused, none applied: version 1 is recorded as " + editedMigration + " sha256:" + sha256Hex(edited)
	if !strings.Contains(output, want) {
		t.Errorf("control: release N's output does not say %q:\n%s", want, output)
	}
}
