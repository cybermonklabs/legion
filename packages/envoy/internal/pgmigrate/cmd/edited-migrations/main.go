// Command edited-migrations is the check before merge that a change edits no migration a database
// may already have run, unless the edit declares the file's earlier identity: for each store,
// Dispatch's and the broker's, it compares every forward migration file the base commit holds with
// this tree's file of the same version (pgmigrate.CheckEdits).
//
//	go run ./internal/pgmigrate/cmd/edited-migrations --base <commit>
//
// The base is the commit the change is merged onto: its merge base with main, or, in CI, the base
// of the pull request's merge commit, which CI checks out. The command reads the base's files with
// git, so the base commit must be in the repository (CI fetches it). It exits 0 when both stores
// compared at least one file and refused none, 1 when a file was refused or a store compared
// nothing, and 2 when the check could not be taken.
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

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// stores are the migration sets the check compares, each a directory of the repository.
var stores = []struct{ name, dir string }{
	{"dispatch", "packages/envoy/internal/dispatch/store/migrations"},
	{"broker", "packages/envoy/internal/broker/store/migrations"},
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
		check, err := checkStore(repository, *base, store.name, store.dir)
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

// checkStore reads the base's forward migrations of dir and this tree's set of dir, and compares
// them.
func checkStore(repository, base, store, dir string) (*pgmigrate.EditCheck, error) {
	listing, err := git("-C", repository, "ls-tree", "-z", "--name-only", base+":"+dir)
	if err != nil {
		return nil, fmt.Errorf("list %s at %s: %w", dir, base, err)
	}
	var files []pgmigrate.BaseFile
	for _, name := range strings.Split(strings.TrimRight(string(listing), "\x00"), "\x00") {
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		text, err := git("-C", repository, "cat-file", "blob", base+":"+path.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", name, base, err)
		}
		files = append(files, pgmigrate.BaseFile{Name: name, Text: text})
	}
	head, err := pgmigrate.Load(os.DirFS(repository), dir)
	if err != nil {
		return nil, fmt.Errorf("load this tree's %s: %w", dir, err)
	}
	return pgmigrate.CheckEdits(store, files, head)
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
