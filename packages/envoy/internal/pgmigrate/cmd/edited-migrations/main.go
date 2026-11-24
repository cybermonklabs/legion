// Command edited-migrations is the check before merge that a change edits no migration a database
// may already have run in a way such a database refuses: for each store, Dispatch's and the
// broker's, it compares every forward migration file the base commit holds with this tree's file
// of the same version (pgmigrate.CheckEdits). A head file that differs from the base's must begin
// its replaces lines with the base's, in the base's order, then one naming the base file itself,
// each line the same file name and checksum: its first line is then the base's first identity
// (the base file itself, where the base declares none), and it accepts every identity and text
// the base's file is or names. A base file whose version the head has no file for is refused.
//
//	go build -o edited-migrations ./internal/pgmigrate/cmd/edited-migrations && ./edited-migrations --base <commit>
//
// The base is the commit the change is merged onto: its merge base with main, or, in CI, the first
// parent of the commit the run checked out, or the bypass run's migration-base. The command reads
// the base's files with git, so the base commit must be in the repository (CI fetches it). It
// exits 0 when both stores compared at least one file and refused none, 1 when a file was refused
// or a store compared nothing, and 2 when the check could not be taken. Built and run, as above,
// it keeps those codes; `go run` reports every failure as exit 1.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"testing/fstest"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// stores are the migration sets the check compares, each a directory of the repository. former is
// the directory a store's set had before it moved, which a base commit from before the move holds
// it under; "" for a store that never moved.
var stores = []struct{ name, dir, former string }{
	{"dispatch", "packages/envoy/internal/dispatch/store/migrations", ""},
	{"broker", "packages/envoy/internal/cipher/store/migrations", "packages/envoy/internal/broker/store/migrations"},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("edited-migrations", flag.ContinueOnError)
	flags.SetOutput(stderr)
	base := flags.String("base", "", "the commit the change is merged onto")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*base) == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: edited-migrations --base <commit>")
		return 2
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintf(stderr, "edited-migrations: find the repository: %v\n", err)
		return 2
	}
	repository := strings.TrimSpace(string(root))
	failed := false
	for _, store := range stores {
		check, err := checkStore(repository, *base, store.name, store.dir, store.former)
		if err != nil {
			fmt.Fprintf(stderr, "edited-migrations: %s: %v\n", store.name, err)
			return 2
		}
		check.Write(stdout)
		if check.Compared == 0 || len(check.Refusals) > 0 {
			failed = true
		}
	}
	if failed {
		fmt.Fprintln(stdout, "edited-migrations: NOT OK")
		return 1
	}
	fmt.Fprintln(stdout, "edited-migrations: ok")
	return 0
}

// checkStore loads the base's set of dir, every file of dir as the base commit holds it, and this
// tree's, both with pgmigrate.Load, and compares them. A base that has no dir but has former, the
// store's directory before it moved, holds the set there. The base's set was merged, so Load
// accepts it; a refusal of it is reported, as a check that could not be taken.
func checkStore(repository, base, store, dir, former string) (*pgmigrate.EditCheck, error) {
	baseDir := dir
	if former != "" {
		if _, err := git("-C", repository, "rev-parse", "--verify", "--quiet", base+":"+dir); err != nil {
			baseDir = former
		}
	}
	listing, err := git("-C", repository, "ls-tree", "-z", "--name-only", base+":"+baseDir)
	if err != nil {
		return nil, fmt.Errorf("list %s at %s: %w", baseDir, base, err)
	}
	files := fstest.MapFS{}
	for _, name := range strings.Split(strings.TrimRight(string(listing), "\x00"), "\x00") {
		text, err := git("-C", repository, "cat-file", "blob", base+":"+path.Join(baseDir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", name, base, err)
		}
		files[path.Join(dir, name)] = &fstest.MapFile{Data: text}
	}
	baseSet, err := pgmigrate.Load(files, dir)
	if err != nil {
		return nil, fmt.Errorf("load the base's %s: %w", dir, err)
	}
	head, err := pgmigrate.Load(os.DirFS(repository), dir)
	if err != nil {
		return nil, fmt.Errorf("load this tree's %s: %w", dir, err)
	}
	return pgmigrate.CheckEdits(store, baseSet, head), nil
}

// git runs git with args and answers its standard output, or an error carrying its standard error.
func git(args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("git", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
