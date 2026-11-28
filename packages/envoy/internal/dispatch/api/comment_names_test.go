package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// gateNames are the comment words this test holds to the code: the names of the checks a route
// passes through, which this package's comments cite more than any other kind of name, and which a
// merge that takes one side of a comment whole is most likely to leave naming something gone. The
// merge of main in #5409 put back an older comment on the write scopes, which named a write-scope
// check that exists nowhere, and nothing read it for three weeks: this test is what reads it.
//
// The names this test cites are written without their leading verb on purpose, so that a citation
// of an example here is not read as a live one.
var gateNames = regexp.MustCompile(`\b((?:refuse|within)[A-Z][A-Za-z0-9]*)\b`)

// TestEveryGateNameACommentNamesExists: a comment of this package that names a refuse… or within…
// check names one that the repository declares. A name declared in another package is allowed,
// because a comment of a route's file cites the checks of the packages a route reaches (docs,
// access, outbox) as readily as its own; a name declared nowhere is a comment that outlived its
// code, and the reader it misleads cannot tell which.
//
// What this test cannot do, so that nobody reads it for more than it is: it does not read what a
// comment CLAIMS. A comment saying a credential naming another project is refused, above the type
// whose rule is the opposite, names only existing things and passes here. That claim is a
// judgment, and a person or an agent reading the two together is what catches it.
func TestEveryGateNameACommentNamesExists(t *testing.T) {
	t.Parallel()
	declared, err := declaredGateNames(repositoryRoot(t))
	if err != nil {
		t.Fatalf("read the repository's declarations: %v", err)
	}
	if len(declared) == 0 {
		t.Fatal("no declaration of a refuse… or within… name anywhere in the repository: this test read nothing, so it could not fail")
	}
	named, err := gateNamesInComments(".")
	if err != nil {
		t.Fatalf("read this package's comments: %v", err)
	}
	if len(named) == 0 {
		t.Fatal("no comment of this package names a refuse… or within… check: this test read nothing")
	}
	var missing []string
	for name, at := range named {
		if !declared[name] {
			missing = append(missing, name+" ("+at+")")
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("a comment of this package names a check the repository declares nowhere:\n\t%s\n"+
			"Name the check the code runs now, or delete the citation.", strings.Join(missing, "\n\t"))
	}
	t.Logf("%d gate names in this package's comments, all declared; %d declared in the repository", len(named), len(declared))
}

// declaredGateNames is every refuse… or within… name the repository declares, in any language: a
// Go func, method, struct field or variable, or a TypeScript function, const or field. Comment
// lines are left out before the declarations are read, so a citation never declares its own name.
// It reads the files git tracks, so an ignored build directory is not read.
func declaredGateNames(root string) (map[string]bool, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "*.go", "*.ts", "*.tsx").Output()
	if err != nil {
		return nil, err
	}
	declaration := regexp.MustCompile(`(?m)^(?:func (?:\([^)]*\) )?|\s*(?:export )?(?:async )?function |\s*(?:export )?(?:const|let|var) |\s+)((?:refuse|within)[A-Z][A-Za-z0-9]*)\s*[\(=:\w\[\]*.]`)
	declared := map[string]bool{}
	for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return nil, err
		}
		for _, match := range declaration.FindAllStringSubmatch(withoutCommentLines(string(content)), -1) {
			declared[match[1]] = true
		}
	}
	return declared, nil
}

// withoutCommentLines is content with each whole-line comment blanked, so that a name a comment
// cites cannot be read as a declaration of itself.
func withoutCommentLines(content string) string {
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "//") || strings.HasPrefix(strings.TrimSpace(line), "*") {
			lines[index] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// gateNamesInComments is every refuse… or within… name the comments of the Go files in dir cite,
// each with the file it is cited in. Only comment text is read: a name in code is the declaration
// or a call, which the compiler holds to the code already.
func gateNamesInComments(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	named := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		for line := range strings.SplitSeq(string(content), "\n") {
			text := strings.TrimSpace(line)
			if !strings.HasPrefix(text, "//") {
				continue
			}
			for _, match := range gateNames.FindAllStringSubmatch(text, -1) {
				if _, cited := named[match[1]]; !cited {
					named[match[1]] = entry.Name()
				}
			}
		}
	}
	return named, nil
}
