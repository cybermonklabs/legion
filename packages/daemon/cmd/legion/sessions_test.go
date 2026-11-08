package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// agentSessions is the sessions directory an agent's Oh My Pi writes under in a pod, the prefix of
// every session file a claim records.
const agentSessions = "/home/legion/.omp/profiles/legion/agent/sessions"

// sessionsImportRig is a session database, a tree volume holding session files, and the claims
// list `legion claims list --json` prints for them.
type sessionsImportRig struct {
	t                       *testing.T
	dsnFile, volume, claims string
}

func newSessionsImportRig(t *testing.T, claims []map[string]string) *sessionsImportRig {
	t.Helper()
	_, dsnFile := testdb.DSNFile(t, "legion_sessions_import_test")
	r := &sessionsImportRig{t: t, dsnFile: dsnFile, volume: t.TempDir(), claims: filepath.Join(t.TempDir(), "claims.json")}
	body, err := json.Marshal(map[string]any{"claims": claims})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.claims, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

// onVolume is where the recorded session file sits on the tree volume, the place the rig writes
// it and the place every refusal of the walk names.
func (r *sessionsImportRig) onVolume(sessionFile string) string {
	return filepath.Join(r.volume, "sessions", strings.TrimPrefix(sessionFile, agentSessions+"/"))
}

// write puts content at the recorded session file's place on the tree volume.
func (r *sessionsImportRig) write(sessionFile, content string) {
	r.t.Helper()
	onVolume := r.onVolume(sessionFile)
	if err := os.MkdirAll(filepath.Dir(onVolume), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(onVolume, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *sessionsImportRig) run(extra ...string) (int, string, string) {
	r.t.Helper()
	var out, errb bytes.Buffer
	args := append([]string{"legion", "sessions", "import", "--dsn-file", r.dsnFile, "--claims", r.claims, "--tree-volume", r.volume}, extra...)
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// refuses imports LEGION-1's claims and reads the whole line printed for the one it could not
// copy. The whole line, because "read the session file" prefixes every refusal the walk prints,
// so a shorter assertion never says which refusal this is.
func (r *sessionsImportRig) refuses(want string, extra ...string) {
	r.t.Helper()
	code, out, _ := r.run(append([]string{"--tree", "LEGION-1"}, extra...)...)
	if code != 1 || !strings.Contains(out, want) {
		r.t.Fatalf("import = %d, stdout\n%s\nwant exit 1 and the line %q", code, out, want)
	}
}

// held is the session the table holds at path, as Oh My Pi reads it.
func (r *sessionsImportRig) held(path string) (string, bool) {
	r.t.Helper()
	conn, err := ompsessions.Connect(context.Background(), r.dsnFile)
	if err != nil {
		r.t.Fatal(err)
	}
	defer conn.Close(context.Background())
	content, ok, err := ompsessions.Read(context.Background(), conn, path)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(content), ok
}

// `legion sessions import` copies each claim's recorded session file from the tree volume into the
// session table under the path the claim recorded, the key Oh My Pi resumes it by, and says per
// claim what it did: copied, nothing recorded, or failed for a file the volume does not hold, which
// exits 1. Run again it copies nothing twice, and a session an agent has since written is refused
// and left as it was.
func TestSessionsImportCopiesEachClaimsSessionOnceAndReportsPerClaim(t *testing.T) {
	architect := agentSessions + "/--legion-workspaces-sjawhar-legion-legion-1--/2026-10-08T12-00-00-000Z_0001.jsonl"
	tester := agentSessions + "/--legion-workspaces-sjawhar-legion-legion-1--/2026-10-08T13-00-00-000Z_0002.jsonl"
	lost := agentSessions + "/--legion-workspaces-sjawhar-legion-legion-1--/2026-10-08T14-00-00-000Z_0003.jsonl"
	other := agentSessions + "/other/2026-10-08T15-00-00-000Z_0004.jsonl"
	r := newSessionsImportRig(t, []map[string]string{
		{"token": "legion-legion-legion-1-tester", "tree": "LEGION-1", "sessionFile": tester, "state": "working"},
		{"token": "legion-legion-legion-1-architect", "tree": "LEGION-1", "sessionFile": architect},
		{"token": "legion-legion-legion-1-planner", "tree": "LEGION-1", "sessionFile": ""},
		{"token": "legion-legion-legion-1-reviewer", "tree": "LEGION-1", "sessionFile": lost},
		{"token": "legion-legion-legion-2-architect", "tree": "LEGION-2", "sessionFile": other},
	})
	r.write(architect, "{\"type\":\"session\",\"id\":\"0001\"}\n")
	r.write(tester, "{\"type\":\"session\",\"id\":\"0002\"}\n{\"type\":\"message\"}\n")

	code, out, errb := r.run("--tree", "LEGION-1")
	if code != 1 || errb != "" {
		t.Fatalf("first import = %d, stderr %q; want 1 for the claim whose file is gone", code, errb)
	}
	for _, want := range []string{
		"legion-legion-legion-1-architect copied " + architect + " (31 bytes, sha256 ",
		"legion-legion-legion-1-planner recorded no session: nothing to copy\n",
		"legion-legion-legion-1-reviewer failed " + lost + ": read the session file: " + r.onVolume(lost) + ": no such file or directory\n",
		"legion-legion-legion-1-tester copied " + tester + " (50 bytes, sha256 ",
		"legion-legion-legion-1-reviewer missing " + lost + ": the session table holds no such session (tree LEGION-1)\n",
		"legion-legion-legion-2-architect missing " + other + ": the session table holds no such session (tree LEGION-2)\n",
		"legion sessions import: copied 2, copied before 0, nothing recorded 1, failed 1, refused 0, missing 2\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("first import printed\n%s\nwant a line containing %q", out, want)
		}
	}
	// The reviewer's directory on the volume holds the other two claims' files, so only its leaf
	// is missing and the line carries the leaf's own refusal. None of the phrases the walk prints
	// for a component it refused belongs on it: that is the rule a reader checks here, and it
	// holds of that line alone, not of the whole output.
	reviewer := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "legion-legion-legion-1-reviewer failed ") {
			reviewer = line
		}
	}
	if reviewer == "" {
		t.Fatalf("first import printed\n%s\nwant a line for the reviewer's unreadable file", out)
	}
	for _, unwanted := range []string{"is a symbolic link", "is not on the tree volume", "is not a directory"} {
		if strings.Contains(reviewer, unwanted) {
			t.Errorf("the reviewer's line %q carries %q, a component's refusal rather than the leaf's", reviewer, unwanted)
		}
	}
	if strings.Contains(out, "legion-legion-legion-2-architect copied") || strings.Contains(out, "legion-legion-legion-2-architect failed") {
		t.Errorf("--tree LEGION-1 copied another tree's claim:\n%s", out)
	}
	if held, ok := r.held(tester); !ok || held != "{\"type\":\"session\",\"id\":\"0002\"}\n{\"type\":\"message\"}\n" {
		t.Errorf("the table holds %q (%t) for the tester, want its file", held, ok)
	}

	r.write(lost, "{\"type\":\"session\",\"id\":\"0003\"}\n")
	code, out, _ = r.run("--tree", "LEGION-1")
	if code != 0 || !strings.Contains(out, "legion-legion-legion-1-architect copied before "+architect+" (identical, 31 bytes") ||
		!strings.Contains(out, "legion sessions import: copied 1, copied before 2, nothing recorded 1, failed 0, refused 0, missing 1\n") {
		t.Fatalf("second import = %d:\n%s\nwant the two copied before, the found one copied, the other tree's still missing, exit 0", code, out)
	}

	r.write(architect, "{\"type\":\"session\",\"id\":\"0001\"}\n{\"type\":\"message\",\"written\":\"after the copy\"}\n")
	code, out, _ = r.run("--tree", "LEGION-1")
	if code != 1 || !strings.Contains(out, "legion-legion-legion-1-architect refused "+architect+": the session table already holds "+architect+" with other content") ||
		!strings.Contains(out, "failed 0, refused 1, missing 1\n") {
		t.Fatalf("import of a changed file = %d:\n%s\nwant the architect refused and counted as refused", code, out)
	}

	// A --tree no claim matches is a mistyped key, never a clean copy.
	if code, _, errb := r.run("--tree", "ACME-404"); code != 1 || !strings.Contains(errb, "has no claim of ACME-404") {
		t.Errorf("import of --tree ACME-404 = %d, %q; want exit 1 naming it", code, errb)
	}
	if code, _, errb := r.run("--claim", "legion-legion-legion-2-architect"); code != 2 || !strings.Contains(errb, "-claim") {
		t.Errorf("import --claim = %d, %q; want the flag refused, as it selects nothing now", code, errb)
	}
	if held, _ := r.held(architect); held != "{\"type\":\"session\",\"id\":\"0001\"}\n" {
		t.Errorf("after the refusal the table holds %q for the architect, want what it held", held)
	}
}

func TestSessionsImportFindsAClosedClaimsSessionByID(t *testing.T) {
	const (
		token            = "legion-legion-legion-1-architect"
		session          = "01a18950-3b61-70b7-82d4-c80464927f8e"
		missingSession   = "01a18950-3b61-70b7-82d4-c80464927f8f"
		duplicateSession = "01a18950-3b61-70b7-82d4-c80464927f90"
	)
	recorded := agentSessions + "/--legion-workspaces-sjawhar-legion-legion-1--/2026-10-29T20-18-19-105Z_" + session + ".jsonl"
	r := newSessionsImportRig(t, []map[string]string{{"token": token, "tree": "LEGION-1", "session": session, "sessionFile": ""}})

	t.Run("one matching Oh My Pi UUIDv7 file imports it", func(t *testing.T) {
		content := "{\"type\":\"session\",\"id\":\"" + session + "\"}\n"
		r.write(recorded, content)
		code, out, errb := r.run("--tree", "LEGION-1")
		if code != 0 || errb != "" || !strings.Contains(out, token+" copied ") {
			t.Fatalf("import = %d, stdout %q, stderr %q; want the closed session copied", code, out, errb)
		}
		if held, ok := r.held(recorded); !ok || held != content {
			t.Fatalf("table holds %q (%t) for the recorded session path, want its file", held, ok)
		}
	})

	t.Run("no matching file names the claim and session", func(t *testing.T) {
		r := newSessionsImportRig(t, []map[string]string{{"token": token, "tree": "LEGION-1", "session": missingSession, "sessionFile": ""}})
		code, out, _ := r.run("--tree", "LEGION-1")
		if code != 1 || !strings.Contains(out, token+" failed session "+missingSession) || !strings.Contains(out, "was not found") {
			t.Fatalf("import = %d, stdout %q; want the missing session named", code, out)
		}
	})

	t.Run("multiple matching files refuse them", func(t *testing.T) {
		r := newSessionsImportRig(t, []map[string]string{{"token": token, "tree": "LEGION-1", "session": duplicateSession, "sessionFile": ""}})
		first := agentSessions + "/one/2026-10-08T12-00-00-000Z_" + duplicateSession + ".jsonl"
		second := agentSessions + "/two/2026-10-08T13-00-00-000Z_" + duplicateSession + ".jsonl"
		r.write(first, "{\"type\":\"session\",\"id\":\""+duplicateSession+"\"}\n")
		r.write(second, "{\"type\":\"session\",\"id\":\""+duplicateSession+"\"}\n")
		code, out, _ := r.run("--tree", "LEGION-1")
		for _, want := range []string{token + " refused session " + duplicateSession, "multiple files", filepath.Join(r.volume, "sessions", "one"), filepath.Join(r.volume, "sessions", "two")} {
			if !strings.Contains(out, want) {
				t.Errorf("import output %q does not name %q", out, want)
			}
		}
		if code != 1 {
			t.Errorf("import = %d, want 1 for duplicate session files", code)
		}
	})

	t.Run("invalid ids cannot shape the search path", func(t *testing.T) {
		for _, id := range []string{session + "/../../outside", session + "*"} {
			if _, err := sessionFileForID(id, r.volume); err == nil || !strings.Contains(err.Error(), "not a UUID") {
				t.Errorf("sessionFileForID(%q) = %v, want the UUID refusal", id, err)
			}
		}
	})
}

func TestSessionsImportRefusesSymlinkedSessionFiles(t *testing.T) {
	const (
		token     = "legion-legion-legion-1-architect"
		session   = "01a18950-3b61-70b7-82d4-c80464927f8e"
		directory = "--legion-workspaces-sjawhar-legion-legion-1--"
	)
	name := "2026-10-29T20-18-19-105Z_" + session + ".jsonl"
	recorded := agentSessions + "/" + directory + "/" + name
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte("outside session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		claim map[string]string
	}{
		{"recorded path", map[string]string{"token": token, "tree": "LEGION-1", "sessionFile": recorded}},
		{"closed claim fallback", map[string]string{"token": token, "tree": "LEGION-1", "session": session, "sessionFile": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSessionsImportRig(t, []map[string]string{tc.claim})
			onVolume := r.onVolume(recorded)
			if err := os.MkdirAll(filepath.Dir(onVolume), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, onVolume); err != nil {
				t.Fatal(err)
			}

			r.refuses(token + " failed " + recorded + ": read the session file: " + onVolume + " is a symbolic link\n")
			if _, found := r.held(recorded); found {
				t.Fatal("the table contains the symlink target")
			}
		})
	}
}

func TestSessionsImportRefusesSymlinkedSessionDirectory(t *testing.T) {
	const (
		token     = "legion-legion-legion-1-architect"
		session   = "01a18950-3b61-70b7-82d4-c80464927f8e"
		directory = "--legion-workspaces-sjawhar-legion-legion-1--"
	)
	name := "2026-10-29T20-18-19-105Z_" + session + ".jsonl"
	recorded := agentSessions + "/" + directory + "/" + name
	// hold puts the session file the link would hand over into the directory it points at.
	hold := func(t *testing.T, target string) {
		t.Helper()
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, name), []byte("outside session\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		claim map[string]string
	}{
		{"recorded path", map[string]string{"token": token, "tree": "LEGION-1", "sessionFile": recorded}},
		{"closed claim fallback", map[string]string{"token": token, "tree": "LEGION-1", "session": session, "sessionFile": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A link out of the volume and one that climbs out of it are the same refusal: the
			// walk reads the component's kind, never where it leads. Each target is made inside
			// the subtest, as the relative one sits on the rig's own volume.
			for _, target := range []struct {
				name string
				make func(t *testing.T, r *sessionsImportRig) string
			}{
				{"absolute target", func(t *testing.T, r *sessionsImportRig) string {
					outside := t.TempDir()
					hold(t, outside)
					return outside
				}},
				{"relative target", func(t *testing.T, r *sessionsImportRig) string {
					hold(t, filepath.Join(r.volume, "outside"))
					return filepath.Join("..", "outside")
				}},
			} {
				t.Run(target.name, func(t *testing.T) {
					r := newSessionsImportRig(t, []map[string]string{tc.claim})
					link := filepath.Join(r.volume, "sessions", directory)
					if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target.make(t, r), link); err != nil {
						t.Fatal(err)
					}

					r.refuses(token + " failed " + recorded + ": read the session file: " + link + " is a symbolic link\n")
					if _, found := r.held(recorded); found {
						t.Fatal("the table contains the symlink-directory target")
					}
				})
			}
		})
	}
}

// Each of these is a path the walk refuses before or during its descent, rather than at the leaf,
// and every refusal names the component on the tree volume it stopped at, never the path the claim
// recorded.
func TestSessionsImportRefusesPathsTheWalkCannotDescend(t *testing.T) {
	const (
		token     = "legion-legion-legion-1-architect"
		session   = "01a18950-3b61-70b7-82d4-c80464927f8e"
		directory = "--legion-workspaces-sjawhar-legion-legion-1--"
	)
	name := "2026-10-29T20-18-19-105Z_" + session + ".jsonl"
	claim := func(recorded string) []map[string]string {
		return []map[string]string{{"token": token, "tree": "LEGION-1", "sessionFile": recorded}}
	}

	t.Run("an intermediate directory the volume does not hold", func(t *testing.T) {
		recorded := agentSessions + "/gone/" + name
		r := newSessionsImportRig(t, claim(recorded))
		if err := os.MkdirAll(filepath.Join(r.volume, "sessions"), 0o700); err != nil {
			t.Fatal(err)
		}
		r.refuses(token + " failed " + recorded + ": read the session file: " +
			filepath.Join(r.volume, "sessions", "gone") + " is not on the tree volume\n")
	})

	t.Run("an intermediate component that is a regular file", func(t *testing.T) {
		recorded := agentSessions + "/a-file/" + name
		r := newSessionsImportRig(t, claim(recorded))
		if err := os.MkdirAll(filepath.Join(r.volume, "sessions"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.volume, "sessions", "a-file"), []byte("not a directory\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Under this kernel the open of a regular file with the walk's flags succeeds, so this
		// refusal can come only from the kind read off the handle just opened, which is why the
		// walk fstats each component it opened rather than reading the name a second time.
		r.refuses(token + " failed " + recorded + ": read the session file: " +
			filepath.Join(r.volume, "sessions", "a-file") + " is not a directory\n")
	})

	t.Run("a path naming the sessions directory itself", func(t *testing.T) {
		recorded := agentSessions + "/."
		r := newSessionsImportRig(t, claim(recorded))
		// The volume holds no sessions directory at all: this refusal comes before the root's
		// own open, and a missing root would be refused in other words.
		r.refuses(token + " failed " + recorded + ": read the session file: " +
			filepath.Join(r.volume, "sessions") + " is the sessions directory itself, not a session file\n")
	})

	t.Run("a path above the sessions directory", func(t *testing.T) {
		recorded := agentSessions + "/.."
		r := newSessionsImportRig(t, claim(recorded))
		// sandbox.SessionOnVolume passes this on, as filepath.Clean("..") is ".." and its guard
		// tests for a "../" prefix alone, so the walk is what refuses it, as the relative path
		// "..", again before the root is opened: this volume too holds no sessions directory.
		r.refuses(token + " failed " + recorded + ": read the session file: " +
			r.volume + " is not under the sessions directory " + filepath.Join(r.volume, "sessions") + "\n")
	})

	t.Run("a sessions directory that cannot be opened", func(t *testing.T) {
		recorded := agentSessions + "/" + directory + "/" + name
		r := newSessionsImportRig(t, claim(recorded))
		missing := filepath.Join(t.TempDir(), "no-such-volume")
		// The rig appends a caller's arguments after its own --tree-volume and flag keeps the
		// last value of a repeated flag, so this runs against a volume that does not exist.
		r.refuses(token+" failed "+recorded+": read the session file: the sessions directory "+
			filepath.Join(missing, "sessions")+" cannot be opened: no such file or directory\n",
			"--tree-volume", missing)
	})
}

// The claims list is the one `legion claims list --json` prints, read from stdin with `-`; anything
// else is refused before the database is opened, as is a missing flag.
func TestSessionsImportRefusesWhatIsNotAClaimsList(t *testing.T) {
	r := newSessionsImportRig(t, nil)
	if err := os.WriteFile(r.claims, []byte(`[{"token":"legion-legion-legion-1-architect"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := r.run(); code != 1 || !strings.Contains(errb, "is not what `legion claims list --json` prints") {
		t.Errorf("import of a bare array = %d, %q; want the refusal", code, errb)
	}
	if err := os.WriteFile(r.claims, []byte(`{"claim":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := r.run(); code != 1 || !strings.Contains(errb, "has no claims member") {
		t.Errorf("import of a list with no claims = %d, %q; want the refusal", code, errb)
	}
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "sessions", "import", "--claims", r.claims}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "--dsn-file is required") {
		t.Errorf("import without --dsn-file = %d, %q; want the usage error", code, errb.String())
	}

	stdin, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(`{"claims":[{"token":"legion-legion-legion-1-planner","tree":"LEGION-1","sessionFile":""}]}`); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	previous := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = previous; _ = stdin.Close() })
	out.Reset()
	if code := run(context.Background(), []string{"legion", "sessions", "import", "--dsn-file", r.dsnFile, "--claims", "-"}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "legion-legion-legion-1-planner recorded no session: nothing to copy") {
		t.Errorf("import from stdin = %d:\n%s", code, out.String())
	}
}
