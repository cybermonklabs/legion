package tmux

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// Under runtime.tmux.session_store postgres a resume of an Oh My Pi session is held to the session
// table, as a pod's launcher holds it: a session the table lacks is refused, never a fresh agent —
// in a database no Oh My Pi has opened yet, and even with a file at its path — before anything is
// written or tmux runs; a session the table holds resumes, with no file at its path at all.
func TestAResumeUnderASessionDatabaseIsHeldToTheSessionTable(t *testing.T) {
	_, dsnFile := testdb.DSNFile(t, "legion_tmux_sessions_test")
	ctx := context.Background()
	stateDir := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{
		Project: "omp", StateDir: stateDir, StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", Launchers: ompOnly("omp"), StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=" + bin},
		Executable: func() (string, error) { return "/opt/legion", nil }, SessionDSNFile: dsnFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	rt.run = func(context.Context, []string) (result, error) { ran++; return result{}, errors.New("tmux ran") }
	prompt := filepath.Join(t.TempDir(), "tester.md")
	if err := os.WriteFile(prompt, []byte("tester"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Prompt.RolePromptPaths = []string{prompt}
	sessions := filepath.Join(stateDir, "home", ".omp", "agent", "sessions")
	held, lacked := filepath.Join(sessions, "held.jsonl"), filepath.Join(sessions, "lacked.jsonl")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lacked, []byte(`{"type":"session","version":3,"id":"lacked"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refusal := func(file string) string {
		return "Refusing to start LEGION-43 fresh while respawning: recorded OMP session is not in the session table: " + file
	}
	resume := func(file string) error {
		resumed := spec
		resumed.ResumeSessionFile = file
		_, err := rt.Resume(ctx, nil, resumed)
		return err
	}

	if err := resume(held); err == nil || err.Error() != refusal(held) {
		t.Errorf("Resume before any Oh My Pi made the table = %v, want %q", err, refusal(held))
	}
	conn, err := ompsessions.Connect(ctx, dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := ompsessions.Import(ctx, conn, held, []byte(`{"type":"session","version":3,"id":"held"}`+"\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := resume(lacked); err == nil || err.Error() != refusal(lacked) {
		t.Errorf("Resume of a session the table lacks, its file present = %v, want %q", err, refusal(lacked))
	}
	if ran != 0 {
		t.Errorf("a refused resume ran tmux %d times", ran)
	}
	if entries, _ := os.ReadDir(filepath.Join(stateDir, "secrets")); len(entries) != 0 {
		t.Errorf("a refused resume wrote %d secret files", len(entries))
	}

	if err := resume(held); ran == 0 {
		t.Errorf("a resume of a session the table holds never reached tmux: %v", err)
	}
}

// Under runtime.tmux.session_store postgres a claim that records its session's id and no file of
// it is looked up in the session table, never in the pane's sessions directory. With no row of the
// id — in a database no Oh My Pi has opened yet, a newer file of the session on this host all the
// same — the session is found nowhere: runtime.ErrSessionFileMissing in the table's own sentence,
// naming the id, which the claim's machine answers by forgetting the session once, as it answers a
// search of the folders that finds no file of it, and no pane opens. With rows of the id the
// pane's command resumes the newest row's path (ompsessions.FindByID), logged as the row found by
// its id, and the file on this host is still never read. A table that cannot be asked is a launch
// failure naming the id, not the session found nowhere, and opens no pane.
func TestAResumeByIDUnderASessionDatabaseReadsTheSessionTable(t *testing.T) {
	_, dsnFile := testdb.DSNFile(t, "legion_tmux_sessions_by_id_test")
	ctx := context.Background()
	var logs strings.Builder
	f := newFakeServerRuntimeWith(t, staticCredential, slog.New(slog.NewTextHandler(&logs, nil)), func(o *Options) {
		o.Environ = append(o.Environ, "HOME="+filepath.Join(o.StateDir, "home"))
		o.SessionDSNFile = dsnFile
	})
	spec := f.spec(t, claim.RoleTester)
	spec.ResumeSession = resumeSessionID
	sessions := paneSessions(f)
	written := time.Date(2026, 11, 19, 22, 12, 27, 829_000_000, time.UTC)
	onHost := filepath.Join(sessions, resumeFolder, "2026-11-20T00-12-27-829Z_"+resumeSessionID+".jsonl")
	writeSession(t, onHost, written.Add(2*time.Hour))

	_, err := f.Resume(ctx, nil, spec)
	want := "resume " + string(spec.Claim) + ": the recorded session file is missing: the session table holds no row of OMP session " + resumeSessionID
	if !errors.Is(err, runtime.ErrSessionFileMissing) || err.Error() != want {
		t.Errorf("Resume by the id before any Oh My Pi made the table = %v, want runtime.ErrSessionFileMissing: %q", err, want)
	}
	if opened(f.argvs) {
		t.Error("a resume by the id that the table holds no row of opened a pane")
	}

	conn, err := ompsessions.Connect(ctx, dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	older := filepath.Join(sessions, resumeFolder, "2026-11-19T22-12-27-829Z_"+resumeSessionID+".jsonl")
	newest := filepath.Join(sessions, "--state-workspaces-acme-widgets-legion-43--", "2026-11-19T23-12-27-829Z_"+resumeSessionID+".jsonl")
	for path, at := range map[string]time.Time{older: written, newest: written.Add(time.Hour)} {
		if _, err := ompsessions.Import(ctx, conn, path, []byte(`{"type":"session","version":3,"id":"`+resumeSessionID+`"}`+"\n"), at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Resume(ctx, nil, spec); err != nil {
		t.Fatalf("Resume by the id of a session the table holds: %v", err)
	}
	if command := openedCommand(f); !strings.Contains(command, "--resume="+newest) || strings.Contains(command, onHost) {
		t.Errorf("the pane's command = %q, want it to resume the table's newest row %s", command, newest)
	}
	resumed := logLines(logs.String(), "tmux runtime: resuming a OMP session the claim recorded no file of, from the session table's row found by its id")
	if len(resumed) != 1 || !strings.Contains(resumed[0], "sessionFile="+newest) {
		t.Errorf("the resume logged %q, want one line naming the row found by its id, %s; log: %s", resumed, newest, logs.String())
	}

	t.Run("a session table that cannot be asked", func(t *testing.T) {
		unanswered := filepath.Join(t.TempDir(), "OMP_SESSION_SQL_DSN")
		if err := os.WriteFile(unanswered, []byte("postgres://legion@127.0.0.1:1/legion?connect_timeout=5\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f := newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) {
			o.Environ = append(o.Environ, "HOME="+filepath.Join(o.StateDir, "home"))
			o.SessionDSNFile = unanswered
		})
		spec := f.spec(t, claim.RoleTester)
		spec.ResumeSession = resumeSessionID
		_, err := f.Resume(ctx, nil, spec)
		want := "spawn " + string(spec.Claim) + ": resume session " + resumeSessionID + ": "
		if err == nil || !strings.HasPrefix(err.Error(), want) || errors.Is(err, runtime.ErrSessionFileMissing) {
			t.Errorf("Resume by the id against a table that does not answer = %v, want a launch failure beginning %q, not runtime.ErrSessionFileMissing", err, want)
		}
		if opened(f.argvs) {
			t.Error("a resume by the id whose table cannot be asked opened a pane")
		}
	})
}

// A resume whose session database does not answer is a launch failure naming the session, not a
// refusal to resume and not a fresh agent: the look-up said nothing about the session.
func TestAResumeWhoseSessionDatabaseDoesNotAnswerFails(t *testing.T) {
	stateDir := t.TempDir()
	dsnFile := filepath.Join(t.TempDir(), "OMP_SESSION_SQL_DSN")
	if err := os.WriteFile(dsnFile, []byte("postgres://legion@127.0.0.1:1/legion?connect_timeout=5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{
		Project: "omp", StateDir: stateDir, StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", Launchers: ompOnly("omp"), StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=" + bin},
		Executable: func() (string, error) { return "/opt/legion", nil }, SessionDSNFile: dsnFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.run = func(context.Context, []string) (result, error) { return result{}, errors.New("tmux ran") }
	prompt := filepath.Join(t.TempDir(), "tester.md")
	if err := os.WriteFile(prompt, []byte("tester"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Prompt.RolePromptPaths = []string{prompt}
	spec.ResumeSessionFile = filepath.Join(stateDir, "session.jsonl")

	_, err = rt.Resume(context.Background(), nil, spec)
	want := "spawn legion-omp-legion-43-tester: resume session " + spec.ResumeSessionFile + ": "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Resume against a database that does not answer = %v, want an error beginning %q", err, want)
	}
}
