package textcheck

//ci:reads packages/envoy/**/*.go !packages/envoy/infra/** !packages/envoy/testdata/** TestEveryTestReadsALockWaitThroughLockwaittest calls lockWaitReads on ../.. (lockWaitRoots), whose walk passes over node_modules, vendor, testdata and dot directories and infra, a directory holding a go.mod of its own, and reads every .go file it finds, test files included
//ci:reads packages/envoy/infra/**/*.go TestEveryTestReadsALockWaitThroughLockwaittest calls lockWaitReads on ../../infra (lockWaitRoots), whose walk reads every .go file under packages/envoy/infra, test files included
//ci:reads packages/daemon/**/*.go TestEveryTestReadsALockWaitThroughLockwaittest calls lockWaitReads on ../../../daemon (lockWaitRoots), whose walk reads every .go file under packages/daemon, test files included

import (
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// lockWaitRoots are the Go modules whose tests read a lock wait, from this directory: Envoy's,
// Envoy's infra and the Legion daemon's. Each is walked alone and each directory holding a go.mod
// of its own is passed over, so each root must yield Go files of its own module.
var lockWaitRoots = []string{"../..", "../../infra", "../../../daemon"}

// lockWaitHelper is the one Go file that spells the lock-wait predicate, from this directory.
const lockWaitHelper = "../../lockwaittest/lockwaittest.go"

// lockWaitTerm is the column the check reads, in the forms the check's documentation names.
const lockWaitTerm = `(?:\w*\s*\(\s*(?:\w+\.)?wait_event(?:_type)?\s*(?:,[^)]*)?\)|(?:\w+\.)?wait_event(?:_type)?)(?:\s*::\s*\w+)?`

// lockWaitValue is Lock, or one of pg_locks' locktype values, which wait_event reports for a lock.
const lockWaitValue = `e?'(?:lock|relation|extend|frozenid|page|tuple|transactionid|virtualxid|spectoken|object|userlock|advisory|applytransaction)'`

// lockWaitOperator is =, <> or !=, or like, ilike and their negations.
const lockWaitOperator = `(?:\s*(?:=|<>|!=)\s*|\s+(?:not\s+)?i?like\s+)`

var (
	// lockWaitEvent is a lockWaitTerm compared with a lockWaitValue in either order, or tested
	// against a list that holds one, in either case and across lines.
	lockWaitEvent = regexp.MustCompile(`(?i)` + lockWaitTerm + lockWaitOperator + lockWaitValue +
		`|` + lockWaitValue + lockWaitOperator + lockWaitTerm +
		`|` + lockWaitTerm + `\s+(?:not\s+)?in\s*\([^)]*` + lockWaitValue)
	// lockWaitPredicate is lockwaittest's select written out, followed by the ) that closes it or
	// by the end of the literal.
	lockWaitPredicate = regexp.MustCompile(`(?i)select\s+pid\s+from\s+pg_locks\s+where\s+not\s+granted(?:\s+and\s+locktype\s*=\s*'\w+')?(?:\s*\)|\s*;?\s*$)`)
)

// No Go file of the three modules reads a lock wait other than through lockwaittest.Waiting or
// WaitingOn. lockwaittest's package documentation says why the wait event is the wrong read and
// what was measured. The check reads every Go string literal, not comments, and names:
//
//   - lockwaittest's predicate written out anywhere but in lockwaittest: the select of the backends
//     pg_locks holds an ungranted request of, with or without one locktype condition and no
//     other, followed by the parenthesis that closes it or by the end of the literal, in whatever
//     statement holds it (pid in, pid not in, exists, a with query);
//   - a wait event compared with a lock by its exact value: wait_event_type with Lock, or
//     wait_event with a locktype such as advisory, by =, <> or !=, or by like, ilike or their
//     negations, in either order, or tested against a list (in, not in) that holds the value; the
//     column qualified (a.wait_event_type) or not, alone, as the first argument of one call
//     (coalesce, lower) or inside one pair of parentheses, and cast after it (::text); the value
//     as a string or as an escape string (E'Lock').
//
// It does not see:
//
//   - the predicate written another way: its conditions in the other order, with a table alias,
//     or as the start of a longer statement;
//   - a wait event matched by a pattern rather than its exact value, such as like 'Lock%';
//   - the column as a later argument of a call, such as coalesce(null, wait_event_type), inside
//     two calls or two pairs of parentheses, inside cast(… as text), or cast with a type that
//     takes an argument, such as ::varchar(10);
//   - the value cast ('Lock'::text), or the column quoted ("wait_event_type");
//   - a list in which a parenthesis comes before the lock value, such as in (lower('x'), 'Lock');
//   - a comparison made in Go, of a wait_event_type the test scanned;
//   - a statement built from pieces: a value passed as a bind parameter, a column and a value in
//     two literals joined by +, or a format verb;
//   - another operator, such as = any(…), or a literal in a testdata directory;
//   - a test that decides on one sample of a wait event other than a lock's.
//
// A read of one lock from pg_locks, by relation or by advisory key, a read of one backend's request
// by its pid, and a read of the waiters behind one backend (pg_blocking_pids) do not write the
// predicate out, and the check leaves them alone. A root that holds no go.mod, or in which the walk
// reads no Go file, fails the check, which would otherwise pass having read nothing.
func TestEveryTestReadsALockWaitThroughLockwaittest(t *testing.T) {
	offences, err := lockWaitReads(lockWaitRoots, lockWaitHelper)
	if err != nil {
		t.Fatal(err)
	}
	if len(offences) > 0 {
		t.Fatalf("%d Go string literals read a lock wait other than through lockwaittest:\n\t%s\n"+
			"read it with lockwaittest.Waiting or lockwaittest.WaitingOn (packages/envoy/lockwaittest)",
			len(offences), strings.Join(offences, "\n\t"))
	}
}

// A verdict is what the check answers for one planted literal.
type verdict string

const (
	compared  verdict = ": a wait event compared with a lock"
	predicate verdict = ": lockwaittest's predicate, written out"
	allowed   verdict = "allowed"    // a read the check leaves alone on purpose
	missed    verdict = "known miss" // a form the check's documentation lists as unseen
)

// The check answers each planted line as its row says: each form it names is named at the line it
// is on, in a raw literal across lines too; each read it leaves alone on purpose and each known
// miss is not named. A known miss is planted so that the list of misses stays true: a change that
// teaches the check one moves its row to the named ones. The helper file, a comment and a module
// below the root are left alone, and a root that holds no go.mod, and one in which it reads no Go
// file, is an error. The planted source splits each form across two literals, so this file holds
// none of them whole.
func TestTheLockWaitCheckSeesWhatItGuards(t *testing.T) {
	root := t.TempDir()
	plant := func(path string, lines ...string) string {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.ToSlash(full)
	}
	const event, typed, locks = "wait_event", "wait_event" + "_type", "pg_" + "locks"
	rows := []struct {
		source  string // one Go line, or more for a raw literal, whose last line holds the form
		verdict verdict
	}{
		{"// A comment that reads " + typed + " = 'Lock' is prose.", allowed},
		{"var plain = \"" + typed + " = 'Lock'\"", compared},
		{"var reversed = \"'Lock' = " + typed + "\"", compared},
		{"var listed = \"" + typed + " in ('LWLock', 'Lock')\"", compared},
		{"var qualified = \"'advisory' <> a." + event + "\"", compared},
		{"var cast = \"coalesce(a." + typed + ", '')::text = 'Lock'\"", compared},
		{"var liked = \"" + typed + " not like 'Lock'\"", compared},
		{"var lowered = \"lower(" + typed + ") = 'lock'\"", compared},
		{"var bracketed = \"'Lock' = (" + typed + ")\"", compared},
		{"var escaped = \"" + typed + " = E'Lock'\"", compared},
		{"var spelled = `select count(*) from pg_stat_activity\n\twhere pid in (select pid from " + locks + " where not granted)`", predicate},
		{"var typedSpelled = \"pid in (select pid from " + locks + " where not granted and locktype = 'advisory')\"", predicate},
		{"var whole = \"select pid from " + locks + " where not granted\"", predicate},
		{"var negated = \"pid not in (select pid from " + locks + " where not granted)\"", predicate},
		{"var exists = \"exists (select pid from " + locks + " where not granted)\"", predicate},
		{"var with = \"with w as (select pid from " + locks + " where not granted) select count(*) from w\"", predicate},
		{"var tail = \"select 1; select pid from " + locks + " where not granted\"", predicate},
		{"var behind = \"select pid from " + locks + " where not granted and pg_backend_pid() = any(pg_blocking_pids(pid))\"", allowed},
		{"var diagnosis = \"select " + typed + ", " + event + " from pg_stat_activity\"", allowed},
		{"var other = \"" + typed + " = 'IO' and " + event + " = 'WalSync'\"", allowed},
		{"var nested = \"lower(trim(" + typed + ")) = 'lock'\"", missed},
		{"var pattern = \"" + typed + " like 'Lock%'\"", missed},
		{"var later = \"coalesce(null, " + typed + ") = 'Lock'\"", missed},
		{"var castAs = \"cast(" + typed + " as text) = 'Lock'\"", missed},
		{"var sized = \"" + typed + "::varchar(10) = 'Lock'\"", missed},
		{"var valueCast = \"'Lock'::text = " + typed + "\"", missed},
		{"var quoted = `\"" + typed + "\" = 'Lock'`", missed},
		{"var alias = \"select l.pid from " + locks + " l where not l.granted\"", missed},
		{"var reordered = \"pid in (select pid from " + locks + " where locktype = 'advisory' and not granted)\"", missed},
	}
	lines, line := []string{"package planted", ""}, 2
	var named []string // ":<line><verdict>" of each row the check must name, at the row's last line
	for _, row := range rows {
		lines = append(lines, row.source)
		line += 1 + strings.Count(row.source, "\n")
		if row.verdict == compared || row.verdict == predicate {
			named = append(named, ":"+strconv.Itoa(line)+string(row.verdict))
		}
	}
	planted := plant("planted.go", lines...)
	helper := plant("lockwaittest/lockwaittest.go", "package lockwaittest", "",
		"const Waiting = \"pid in (select pid from "+locks+" where not granted)\"")
	plant("go.mod", "module planted")
	plant("nested/go.mod", "module nested")
	plant("nested/nested.go", "package nested", "", "var plain = \""+typed+" = 'Lock'\"")

	got, err := lockWaitReads([]string{root}, helper)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, n := range named {
		want = append(want, planted+n)
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the check found\n\t%s\nwant\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}

	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, "go.mod"), []byte("module empty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lockWaitReads([]string{root, empty}, helper); err == nil || !strings.Contains(err.Error(), "read no Go file under "+empty) {
		t.Errorf("a module root holding no Go file answered %v, want an error naming it", err)
	}
	inside := filepath.Join(root, "lockwaittest")
	if _, err := lockWaitReads([]string{inside}, helper); err == nil || !strings.Contains(err.Error(), "read no module at "+inside) {
		t.Errorf("a root that holds Go files and no go.mod answered %v, want an error naming it", err)
	}
}

// lockWaitReads names, as path:line: what, sorted, each Go string literal under roots that compares
// a wait event with a lock (lockWaitEvent) or writes out lockwaittest's predicate
// (lockWaitPredicate), the file helper passed over for the second. Each root must hold a go.mod,
// as a module's root does, and is walked alone, its node_modules, vendor, testdata and dot
// directories and each directory below it holding a go.mod passed over, and a root that yields
// no Go file is an error. Comments are not literals, so prose about a read is not read.
func lockWaitReads(roots []string, helper string) ([]string, error) {
	skip, err := filepath.Abs(helper)
	if err != nil {
		return nil, err
	}
	var offences []string
	for _, root := range roots {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
			return nil, fmt.Errorf("read no module at %s: it holds no go.mod, so it is not the root of a module whose Go files the check reads: %w", root, err)
		}
		read := 0
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path == root {
					return nil
				}
				if name := entry.Name(); name == "node_modules" || name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			source, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			read++
			absolute, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			offences = append(offences, lockWaitReadsIn(path, source, absolute == skip)...)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("read the Go files under %s: %w", root, err)
		}
		if read == 0 {
			return nil, fmt.Errorf("read no Go file under %s: the check would pass having seen nothing of that module", root)
		}
	}
	slices.Sort(offences)
	return offences, nil
}

// lockWaitReadsIn is lockWaitReads for the string literals of one file's source. A match in a raw
// literal is named at its own line; one in an interpreted literal, which holds no line break, at
// the literal's.
func lockWaitReadsIn(path string, source []byte, helper bool) []string {
	files := token.NewFileSet()
	file := files.AddFile(path, files.Base(), len(source))
	var lex scanner.Scanner
	lex.Init(file, source, nil, 0)
	var offences []string
	for {
		position, kind, literal := lex.Scan()
		if kind == token.EOF {
			return offences
		}
		if kind != token.STRING {
			continue
		}
		text, err := strconv.Unquote(literal)
		if err != nil {
			text = literal
		}
		line := files.Position(position).Line
		name := func(at int, what string) {
			if strings.HasPrefix(literal, "`") {
				at = line + strings.Count(text[:at], "\n")
			} else {
				at = line
			}
			offences = append(offences, filepath.ToSlash(path)+":"+strconv.Itoa(at)+": "+what)
		}
		for _, match := range lockWaitEvent.FindAllStringIndex(text, -1) {
			name(match[0], "a wait event compared with a lock")
		}
		if !helper {
			for _, match := range lockWaitPredicate.FindAllStringIndex(text, -1) {
				name(match[0], "lockwaittest's predicate, written out")
			}
		}
	}
}
