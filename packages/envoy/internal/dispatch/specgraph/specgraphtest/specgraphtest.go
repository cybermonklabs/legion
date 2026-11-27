// Package specgraphtest is test support for the tests of more than one package that read the
// scenarios of specgraph's testdata or write trees for git: specgraph's own tests and the
// spec-check command's tests. It is an ordinary package rather than a _test.go file because those
// tests are of two packages, and neither can import the other's test helpers, the reason
// internal/cmdtest gives for itself. Only test files import it (internal/textcheck's
// TestOnlyTestFilesImportSpecgraphtest), and besides testexec, whose WriteFile writes the
// executable entries a test then runs under the fork lock and which does not import specgraph, it
// imports the standard library alone, so the tests of package specgraph can import it with no
// cycle.
package specgraphtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/envoy/testexec"
)

// PutTree writes each entry of files under dir: "file <bytes>" a file, "exec <bytes>" an
// executable file, and "link <target>" a link.
func PutTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, entry := range files {
		kind, value, _ := strings.Cut(entry, " ")
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		var err error
		switch kind {
		case "file":
			err = os.WriteFile(full, []byte(value), 0o644)
		case "exec":
			err = testexec.WriteFile(full, []byte(value), 0o755)
		case "link":
			err = os.Symlink(value, full)
		default:
			t.Fatalf("an entry of kind %q", kind)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// testAttributes is each attribute of a check's block that names the test of a program, which a
// check of another procedure does not hold.
var testAttributes = []string{"runner", "selector", "job"}

// subjectRead is, for a check of production and for one of the graph, the member of a result that
// names the subject that the check reads, the value that As writes there (the deploy d1 and the
// transaction tx1 of production-and-graph's subject), and the member of the other of the two
// subjects, which As takes out.
var subjectRead = map[string]struct{ member, value, other string }{
	"production": {"deploy", "d1", "graph_tx"},
	"graph":      {"graph_tx", "tx1", "deploy"},
}

// As is the scenario at the root of fsys with its check id written as a check of procedure and
// subject, which a test reads as a check of that kind. In each approved version of the check's
// block, and in its block in each spec file (files/<commit>/spec.md): the attributes procedure and
// subject set to procedure and subject, and the attributes that name a program's test (runner,
// selector and job) taken out when procedure is no program. As writes no such attribute: a check
// written as a program keeps the test that its block names, as production-and-graph's program check
// of production does, and the reader refuses a program check that names none. For a check of
// production, each result of it that read a transaction of the graph reads the deploy d1 instead,
// as a result of production reads a deploy; for a check of the graph, each result of it that read a
// deploy reads the transaction tx1 instead, as a result of the graph reads a transaction. For a
// query or an agent of the code, each failing example of it that names the commit that a step
// planted its fault on (planted_on) reads that commit as its commit, with no planted_on and no
// fault_paths, as a failing example of such a check counts at its commit: only a program check of
// the code counts one from planted_on. Each other file holds the bytes of fsys. As refuses a
// scenario whose approved spec holds no node id, a spec file that does not hold the check's block
// once, and a block that sets no procedure or no subject, so that no test passes on a change that
// changed nothing.
func As(fsys fs.FS, id, procedure, subject string) (fstest.MapFS, error) {
	out := fstest.MapFS{}
	err := fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, path)
		out[path] = &fstest.MapFile{Data: data}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("specgraphtest: %w", err)
	}
	written := func(block string) (string, error) { return writtenAs(block, procedure, subject) }
	if err := editJSON(out, "approved.json", func(v any) error { return editVersions(v, id, written) }); err != nil {
		return nil, err
	}
	specs, err := fs.Glob(out, "files/*/spec.md")
	if err != nil || len(specs) == 0 {
		return nil, fmt.Errorf("specgraphtest: no spec file under files/ (%v)", err)
	}
	for _, path := range specs {
		text := string(out[path].Data)
		header := ":::check{#" + id + " "
		if n := strings.Count(text, header); n != 1 {
			return nil, fmt.Errorf("specgraphtest: %s holds the block of %s %d times, want once", path, id, n)
		}
		start := strings.Index(text, header)
		end := strings.Index(text[start:], "\n:::")
		if end < 0 {
			return nil, fmt.Errorf("specgraphtest: %s: the block of %s has no end", path, id)
		}
		block, err := writtenAs(text[start:start+end], procedure, subject)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out[path] = &fstest.MapFile{Data: []byte(text[:start] + block + text[start+end:])}
	}
	read, reads := subjectRead[subject]
	planted := subject == "code" && procedure != "program"
	if !reads && !planted {
		return out, nil
	}
	err = editJSON(out, "results.json", func(v any) error {
		results, ok := v.([]any)
		if !ok {
			return fmt.Errorf("it is no list")
		}
		for _, r := range results {
			switch r, ok := r.(map[string]any); {
			case !ok || r["check"] != id:
			case reads && r[read.other] != nil:
				delete(r, read.other)
				r[read.member] = read.value
			case planted && r["kind"] == "failing_example" && r["planted_on"] != nil:
				r["commit"] = r["planted_on"]
				delete(r, "planted_on")
				delete(r, "fault_paths")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// attribute is the attribute name of a block's first line and its value.
func attribute(name string) *regexp.Regexp {
	return regexp.MustCompile(` ` + name + `="[^"]*"`)
}

// writtenAs is the block of a check, its first line and the lines after it, written as a check of
// procedure and subject (As): only its first line changes.
func writtenAs(block, procedure, subject string) (string, error) {
	end := strings.IndexByte(block, '\n')
	if end < 0 {
		end = len(block)
	}
	header := block[:end]
	if !attribute("procedure").MatchString(header) || !attribute("subject").MatchString(header) {
		return "", fmt.Errorf("specgraphtest: the block %q sets no procedure or no subject", header)
	}
	header = attribute("procedure").ReplaceAllLiteralString(header, ` procedure="`+procedure+`"`)
	header = attribute("subject").ReplaceAllLiteralString(header, ` subject="`+subject+`"`)
	if procedure != "program" {
		for _, name := range testAttributes {
			header = attribute(name).ReplaceAllLiteralString(header, "")
		}
	}
	return header + block[end:], nil
}

// editVersions changes the block of each version of the node id of the approved spec v by write.
func editVersions(v any, id string, write func(string) (string, error)) error {
	file, _ := v.(map[string]any)
	nodes, _ := file["nodes"].([]any)
	found := false
	for _, n := range nodes {
		n, _ := n.(map[string]any)
		if n["id"] != id {
			continue
		}
		found = true
		versions, _ := n["versions"].([]any)
		for _, version := range versions {
			version, _ := version.(map[string]any)
			block, _ := version["block"].(string)
			written, err := write(block)
			if err != nil {
				return err
			}
			version["block"] = written
		}
	}
	if !found {
		return fmt.Errorf("it holds no node %q", id)
	}
	return nil
}

// editJSON changes the JSON file name of fsys by change, which takes its tree and changes it in
// place. Numbers keep their text.
func editJSON(fsys fstest.MapFS, name string, change func(any) error) error {
	file, held := fsys[name]
	if !held {
		return fmt.Errorf("specgraphtest: the scenario has no %s", name)
	}
	dec := json.NewDecoder(bytes.NewReader(file.Data))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return fmt.Errorf("specgraphtest: %s: %w", name, err)
	}
	if err := change(tree); err != nil {
		return fmt.Errorf("specgraphtest: %s: %w", name, err)
	}
	data, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("specgraphtest: %s: %w", name, err)
	}
	fsys[name] = &fstest.MapFile{Data: data}
	return nil
}
