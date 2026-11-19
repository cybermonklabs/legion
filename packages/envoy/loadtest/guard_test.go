package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// isolatePG keeps a test's -pg values from the environment: pgconn.ParseConfig, which the guard
// and meta.json read -pg with, takes defaults from the PG* variables and from files under $HOME.
func isolatePG(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGSERVICE", "PGSERVICEFILE",
		"PGSSLMODE", "PGSSLKEY", "PGSSLCERT", "PGSSLROOTCERT", "PGSSLPASSWORD",
	} {
		t.Setenv(name, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PGPASSFILE", filepath.Join(home, "absent.pgpass"))
}

func testStack() *stackFile {
	return &stackFile{
		Name: "loadtest-abc", DispatchToken: "agent-secret", ListenerToken: "listener-secret",
		Ports: stackPorts{Postgres: 15432, Proxy: 15433, ProxyStats: 15434, NATS: 14222, NATSMonitor: 18222, Listener: 19120, Dispatch: 18777, Pprof: 16061},
		PIDs:  stackPIDs{Postgres: 11, Dispatch: 12},
	}
}

func TestGuardRefusesAFlagThatDiffersFromTheStack(t *testing.T) {
	// The tokens come from their files alone, so a token that differs is refused naming the file
	// flag that gave it.
	cases := map[string]endpointFlags{
		"-url":                 {url: "http://127.0.0.1:9020"},
		"-listener":            {listener: "http://127.0.0.1:9020"},
		"-token-file":          {token: "some-other-token"},
		"-listener-token-file": {listenerToken: "some-other-token"},
		"-pg":                  {pg: "postgres://postgres@127.0.0.1:5432/dispatch"},
	}
	for flag, flags := range cases {
		_, err := resolveEndpoints(flags, testStack())
		var foreign *foreignFlagError
		if !errors.As(err, &foreign) {
			t.Fatalf("%s: not refused as a foreign flag: %v", flag, err)
		}
		if foreign.Flag != flag {
			t.Errorf("refusal names %s, want %s", foreign.Flag, flag)
		}
	}
}

func TestGuardTakesEndpointsFromTheStack(t *testing.T) {
	isolatePG(t)
	e, err := resolveEndpoints(endpointFlags{url: "http://127.0.0.1:18777/"}, testStack())
	if err != nil {
		t.Fatalf("a flag equal to the stack's value was refused: %v", err)
	}
	if e.dispatch != "http://127.0.0.1:18777" || e.pg != "postgres://postgres@127.0.0.1:15432/dispatch?sslmode=disable" ||
		e.listenerToken != "listener-secret" || e.natsServerName != "loadtest-abc" {
		t.Fatalf("endpoints %+v do not come from the stack", e)
	}
	if e.pids["postgres"] != 11 || e.pids["dispatch"] != 12 {
		t.Fatalf("pids %v, want the stack's", e.pids)
	}
	if got := redactEndpoint("-pg", e.pg); got != "postgres://127.0.0.1:15432/dispatch" {
		t.Fatalf("the stack's -pg recorded as %q, want its host, port and database", got)
	}
	if e.proxyStats != "http://127.0.0.1:15434" {
		t.Fatalf("proxy stats %q, want the stack's proxy_stats port", e.proxyStats)
	}
}

// foreignFlags gives every endpoint flag a foreign value.
func foreignFlags() endpointFlags {
	return endpointFlags{
		url: "http://dispatch.example:8080/", listener: "http://listener.example:9120", nats: "nats://nats.example:4222",
		natsMon: "http://nats.example:8222", pprof: "http://dispatch.example:6061", pg: "postgres://postgres@db.example:5432/dispatch",
		token: "cluster-agent-token", listenerToken: "cluster-listener-token", allowForeign: true,
	}
}

func TestGuardAllowForeignUsesTheFlags(t *testing.T) {
	e, err := resolveEndpoints(foreignFlags(), testStack())
	if err != nil {
		t.Fatal(err)
	}
	if e.dispatch != "http://dispatch.example:8080" || e.listener != "http://listener.example:9120" || e.pg != "postgres://postgres@db.example:5432/dispatch" ||
		e.pprof != "http://dispatch.example:6061" || e.natsMon != "http://nats.example:8222" || e.token != "cluster-agent-token" {
		t.Fatalf("endpoints %+v: want every one from the flags", e)
	}
	if len(e.pids) != 0 || e.natsServerName != "" {
		t.Fatalf("a foreign target keeps pids %v and server name %q, want neither", e.pids, e.natsServerName)
	}
	// The proxy's wire counts are the local stack's: a foreign run reads none.
	if e.proxyStats != "" {
		t.Fatalf("a foreign target keeps the stack's proxy stats %q, want none", e.proxyStats)
	}
}

func TestGuardAllowForeignRefusesAMissingFlag(t *testing.T) {
	unsetters := map[string]func(*endpointFlags){
		"-url":                 func(f *endpointFlags) { f.url = "" },
		"-listener":            func(f *endpointFlags) { f.listener = "" },
		"-nats":                func(f *endpointFlags) { f.nats = "" },
		"-nats-mon":            func(f *endpointFlags) { f.natsMon = "" },
		"-pprof":               func(f *endpointFlags) { f.pprof = "" },
		"-pg":                  func(f *endpointFlags) { f.pg = "" },
		"-token-file":          func(f *endpointFlags) { f.token = "" },
		"-listener-token-file": func(f *endpointFlags) { f.listenerToken = "" },
	}
	for flag, unset := range unsetters {
		for _, st := range []*stackFile{testStack(), nil} {
			flags := foreignFlags()
			unset(&flags)
			_, err := resolveEndpoints(flags, st)
			var missing *missingForeignFlagError
			if !errors.As(err, &missing) {
				t.Fatalf("%s left empty (stack.json present: %t): not refused as a missing flag: %v", flag, st != nil, err)
			}
			if missing.Flag != flag {
				t.Errorf("refusal names %s, want %s", missing.Flag, flag)
			}
		}
	}
	// With -url and -token-file only, the refusal names the first flag left empty.
	_, err := resolveEndpoints(endpointFlags{url: "http://127.0.0.1:18777", token: "x", allowForeign: true}, testStack())
	var missing *missingForeignFlagError
	if !errors.As(err, &missing) || missing.Flag != "-listener" {
		t.Fatalf("refusal %v, want -listener named", err)
	}
}

// credentialFileAt is a credential file of mode 600 holding content, as vm/vm.sh writes one on
// the machine the command that reads it runs on, and its path. Every command of this package
// that takes a credential reads its file through readCredentials, so every test of one writes
// the file here.
func credentialFileAt(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// credentialFiles is every endpoint flag of a foreign run, with the three that carry a secret
// given as files alone. Each file's content ends in white space, as a file written with printf
// does (vm/vm.sh writes these on the machine the load runs from).
func credentialFiles(t *testing.T) endpointFlags {
	t.Helper()
	f := foreignFlags()
	f.token, f.listenerToken, f.pg = "", "", ""
	f.tokenFile = credentialFileAt(t, "token", "cluster-agent-token\n")
	f.listenerTokenFile = credentialFileAt(t, "listener-token", "cluster-listener-token \t\n")
	f.pgFile = credentialFileAt(t, "pg", "postgres://postgres:pw@db.example:5432/dispatch\n\n")
	return f
}

// TestGuardReadsCredentialFiles: each credential that carries a secret is the content of the
// file its -…-file flag names, trailing white space trimmed, and the file form alone satisfies
// -allow-foreign, so a caller gives the three on no command line at all.
func TestGuardReadsCredentialFiles(t *testing.T) {
	isolatePG(t)
	e, err := resolveEndpoints(credentialFiles(t), testStack())
	if err != nil {
		t.Fatalf("a foreign run given its three credentials as files was refused: %v", err)
	}
	if e.token != "cluster-agent-token" || e.listenerToken != "cluster-listener-token" ||
		e.pg != "postgres://postgres:pw@db.example:5432/dispatch" {
		t.Fatalf("endpoints %+v: want each credential its file's content, trailing white space trimmed", e)
	}
}

// TestGuardRefusesBothCredentialForms: -pg, the one credential whose value form stays, given
// with -pg-file too is refused naming the two forms. A token flag given with its file is refused
// as a removed flag (TestACredentialOnTheCommandLineIsRefused).
func TestGuardRefusesBothCredentialForms(t *testing.T) {
	isolatePG(t)
	flags := credentialFiles(t)
	flags.pg = "postgres://postgres:pw@db.example:5432/dispatch"
	_, err := resolveEndpoints(flags, testStack())
	var both *bothCredentialFormsError
	if !errors.As(err, &both) {
		t.Fatalf("-pg given with its file too: not refused as one credential in two forms: %v", err)
	}
	if both.Flag != "-pg" || both.FileFlag != "-pg-file" {
		t.Errorf("refusal names %s and %s, want -pg and -pg-file", both.Flag, both.FileFlag)
	}
}

func TestGuardRefusesAnUnusableCredentialFile(t *testing.T) {
	isolatePG(t)
	absent := filepath.Join(t.TempDir(), "absent-credential")
	setters := map[string]func(*endpointFlags){
		"-token-file":          func(f *endpointFlags) { f.tokenFile = absent },
		"-listener-token-file": func(f *endpointFlags) { f.listenerTokenFile = absent },
		"-pg-file":             func(f *endpointFlags) { f.pgFile = absent },
	}
	for flag, set := range setters {
		flags := credentialFiles(t)
		set(&flags)
		_, err := resolveEndpoints(flags, testStack())
		if err == nil || !strings.Contains(err.Error(), flag) || !strings.Contains(err.Error(), absent) {
			t.Errorf("%s naming a file that is not there: refusal %v, want one naming the flag and the path", flag, err)
		}
	}
	// A file of white space holds no credential. It is refused by name here rather than left to
	// -allow-foreign, which would report the flag as one nobody gave.
	empty := filepath.Join(t.TempDir(), "empty-credential")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	flags := credentialFiles(t)
	flags.tokenFile = empty
	_, err := resolveEndpoints(flags, testStack())
	if err == nil || !strings.Contains(err.Error(), "-token-file") || !strings.Contains(err.Error(), empty) {
		t.Errorf("an empty -token-file: refusal %v, want one naming the flag and the path", err)
	}
}

// TestGuardPrintsNoURLPassword: a foreign URL that does not parse is refused naming its flag
// alone, and a failed guard check names the endpoint without its userinfo, user or password.
func TestGuardPrintsNoURLPassword(t *testing.T) {
	isolatePG(t)
	const password = "url-password-51d2"
	flags := foreignFlags()
	flags.pg = "host=db.example user=postgres password=" + password
	if _, err := resolveEndpoints(flags, testStack()); err == nil || !strings.Contains(err.Error(), "-pg") || strings.Contains(err.Error(), password) {
		t.Fatalf("a -pg that is not a URL: refusal %v, want one naming -pg without its password", err)
	}
	// pgx reads a -pg value as a URL only when it starts with postgres:// or postgresql://
	// (case-sensitive), and an invalid port fails its parse: each is refused by name, with no
	// part of the value and none of pgx's error.
	for _, pg := range []string{"POSTGRES://u:pw-frag1@h/db", "pg://u:pw-frag2@h/db", "postgres://u:pw-frag3@h:notaport/db"} {
		flags := foreignFlags()
		flags.pg = pg
		_, err := resolveEndpoints(flags, testStack())
		if err == nil || !strings.Contains(err.Error(), "-pg") {
			t.Errorf("-pg %q: refusal %v, want one naming -pg", pg, err)
			continue
		}
		for _, part := range []string{"pw-frag", "u:", "@h", "notaport", "parse"} {
			if strings.Contains(err.Error(), part) {
				t.Errorf("-pg %q: refusal %q holds %q", pg, err, part)
			}
		}
	}
	flags = foreignFlags()
	flags.nats = "nats://tok@127.0.0.1:3,nats://u:" + password + "@127.0.0.1:4"
	if _, err := resolveEndpoints(flags, testStack()); err != nil {
		t.Fatalf("a -nats server list: refused %v, want it accepted", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	withPassword := strings.Replace(srv.URL, "://", "://loadgen:"+password+"@", 1)
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, e := range []endpoints{
		{natsMon: withPassword, natsServerName: "loadtest-abc"},
		{dispatch: withPassword},
	} {
		err := checkStack(context.Background(), e)
		if err == nil || strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "loadgen") || !strings.Contains(err.Error(), host) {
			t.Errorf("guard check against %+v: error %v, want one naming the endpoint's host without its userinfo", e, err)
		}
	}
}

// TestACredentialOnTheCommandLineIsRefused: every argument of a process is readable by every
// user of its machine (/proc/<pid>/cmdline, `ps -eo args`) for as long as the process runs, so
// -token, -listener-token and stub-embed's -key are removed. Each one given, in either spelling
// and whatever else is given with it, is refused by the command before its first connection,
// naming the -…-file form to give instead, and the refusal holds nothing of the value.
func TestACredentialOnTheCommandLineIsRefused(t *testing.T) {
	isolatePG(t)
	t.Setenv("LOADTEST_ROOT", t.TempDir())
	const secret = "argv-secret-7d1f"
	tokenFile := credentialFileAt(t, "dispatch-token", "agent-token\n")
	listenerFile := credentialFileAt(t, "listener-token", "listener-token\n")
	// foreign is every endpoint flag of a foreign run but the two tokens, each a closed port, so a
	// command that took the credential fails at its first connection instead of at the refusal.
	foreign := func(extra ...string) []string {
		return append([]string{
			"-allow-foreign", "-url", "http://127.0.0.1:1", "-listener", "http://127.0.0.1:1", "-nats", "nats://127.0.0.1:1",
			"-nats-mon", "http://127.0.0.1:1", "-pprof", "http://127.0.0.1:1", "-pg", "postgres://postgres@127.0.0.1:1/dispatch",
		}, extra...)
	}
	run := func(extra ...string) []string {
		return foreign(append([]string{"-events", "0", "-sessions", "1", "-requests", "0", "-out", t.TempDir()}, extra...)...)
	}
	stubEmbed := func(args []string) error {
		_, err := stubEmbedArgs(args)
		return err
	}
	for _, c := range []struct {
		command        string
		run            func([]string) error
		args           []string
		flag, fileFlag string
	}{
		{"run", runLoad, run("-token", secret, "-listener-token-file", listenerFile), "-token", "-token-file"},
		{"run", runLoad, run("-token="+secret, "-listener-token-file", listenerFile), "-token", "-token-file"},
		{"run", runLoad, run("-token", secret, "-token-file", tokenFile, "-listener-token-file", listenerFile), "-token", "-token-file"},
		{"run", runLoad, run("-token-file", tokenFile, "--listener-token", secret), "-listener-token", "-listener-token-file"},
		{"seed", runSeed, foreign("-token", secret, "-listener-token-file", listenerFile), "-token", "-token-file"},
		{"seed-refs", runSeedRefs, foreign("-token-file", tokenFile, "-listener-token="+secret), "-listener-token", "-listener-token-file"},
		{"seed-finish", runSeedFinish, []string{"-pg", "postgres://postgres@127.0.0.1:1/dispatch", "-url", "http://127.0.0.1:1", "-token", secret}, "-token", "-token-file"},
		{"stub-embed", stubEmbed, []string{"-key", secret}, "-key", "-key-file"},
		{"stub-embed", stubEmbed, []string{"-key=" + secret, "-listen", ""}, "-key", "-key-file"},
		// The flag package stops at the first argument that is not a flag, and at "--": a removed
		// flag after either is refused all the same (parseArgs), naming its file form, and the
		// command does not go on to refuse the stray argument or to read a file instead.
		{"run", runLoad, run("-token-file", tokenFile, "-listener-token-file", listenerFile, "stray", "-token", secret), "-token", "-token-file"},
		{"replay-checksum", runReplayChecksum, []string{"--", "--listener-token=" + secret}, "-listener-token", "-listener-token-file"},
		{"replay-check-seed", runReplayCheckSeed, []string{"stray", "-token"}, "-token", "-token-file"},
		{"stub-embed", stubEmbed, []string{"-key-file", filepath.Join(t.TempDir(), "absent"), "--", "-key", secret}, "-key", "-key-file"},
	} {
		err := c.run(c.args)
		var removed *removedFlagError
		if !errors.As(err, &removed) || removed.Flag != c.flag || removed.FileFlag != c.fileFlag {
			t.Errorf("loadgen %s %q: error %v, want %s refused naming %s", c.command, c.args, err, c.flag, c.fileFlag)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, c.fileFlag) || strings.Contains(msg, secret) {
			t.Errorf("loadgen %s %q: refusal %q, want one naming %s and holding nothing of the value", c.command, c.args, msg, c.fileFlag)
		}
	}
}

// TestACommandLineSecretIsRefusedBeforeAnyOtherInput: the refusal of a secret on the command line
// comes right after the parse (parseArgs), before the command checks, reads or opens anything
// else. A command that read its -plan or -bundle first would stay blocked on a slow or never-ending
// file with the secret in its argv, and one that refused a missing flag first would never name the
// file form.
func TestACommandLineSecretIsRefusedBeforeAnyOtherInput(t *testing.T) {
	isolatePG(t)
	t.Setenv("LOADTEST_ROOT", t.TempDir())
	const secret = "argv-secret-3b9e"
	absent := filepath.Join(t.TempDir(), "absent")
	for _, c := range []struct {
		command string
		run     func([]string) error
		args    []string
	}{
		// Each would fail on its first other input: a -plan or -bundle that is not there, a
		// required flag left out.
		{"loop-seed-write", runLoopSeedWrite, []string{"-token", secret, "-plan", absent}},
		{"loop-seed-write", runLoopSeedWrite, []string{"-pg", "postgres://u:" + secret + "@h/db", "-plan", absent}},
		{"replay-check-seed", runReplayCheckSeed, []string{"-listener-token", secret, "-bundle", absent}},
		{"replay-seed", runReplaySeed, []string{"-token", secret, "-bundle", absent}},
		{"replay-checksum", runReplayChecksum, []string{"-token", secret}},
		{"replay-logical-dump", runReplayLogicalDump, []string{"-token", secret}},
		{"refs-fetch", runRefsFetch, []string{"-listener-token", secret}},
		{"pg-size", runPGSize, []string{"-pg", "postgres://u:" + secret + "@h/db"}},
		{"loop-seed-check", runLoopSeedCheck, []string{"-pg", "postgres://u:" + secret + "@h/db"}},
		{"stub-embed", runStubEmbed, []string{"-key", secret, "-listen", ""}},
	} {
		err := c.run(c.args)
		var removed *removedFlagError
		var password *pgPasswordError
		if !errors.As(err, &removed) && !errors.As(err, &password) {
			t.Errorf("loadgen %s %q: error %v, want the secret refused before any other input", c.command, c.args, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("loadgen %s %q: refusal %q holds the value", c.command, c.args, err)
		}
	}
	// A -pg naming a file that blocks its reader -- a FIFO nobody writes, as its sslrootcert -- is
	// judged as text, so the parse never waits on that file with a secret in the argv: neither a
	// -pg with a password nor a removed flag beside a -pg with none.
	fifo := filepath.Join(t.TempDir(), "blocked-ca.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	blocking := "host=127.0.0.1 port=1 user=postgres sslmode=verify-full sslrootcert=" + fifo
	for _, args := range [][]string{
		{"-token", secret, "-pg", blocking},
		{"-pg", blocking + " password=" + secret},
	} {
		done := make(chan error, 1)
		go func() { done <- runSeedFinish(args) }()
		select {
		case err := <-done:
			var removed *removedFlagError
			var password *pgPasswordError
			if !errors.As(err, &removed) && !errors.As(err, &password) {
				t.Errorf("loadgen seed-finish %q: error %v, want the secret refused", args, err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("loadgen seed-finish %q: still running after 10 s, waiting on %s with the secret in its argv", args, fifo)
		}
	}
}

// TestAStrayArgumentIsRefused: the replay commands take no positional argument, and each refuses
// one right after the parse, before it reads its bundle or connects, as every other command of
// this package does. A credential flag after the stray argument is refused first, naming its file
// form (TestACredentialOnTheCommandLineIsRefused).
func TestAStrayArgumentIsRefused(t *testing.T) {
	isolatePG(t)
	for command, run := range map[string]func([]string) error{
		"replay-check-seed":   runReplayCheckSeed,
		"replay-checksum":     runReplayChecksum,
		"replay-logical-dump": runReplayLogicalDump,
	} {
		if err := run([]string{"stray"}); err == nil || err.Error() != `unexpected argument "stray"` {
			t.Errorf("loadgen %s stray: error %v, want the stray argument refused", command, err)
		}
	}
}

// TestTheTokenFilesAreRead: what a command reads in place of a token flag is the file its
// -…-file form names, trailing white space trimmed, given on the command line as a caller gives it.
func TestTheTokenFilesAreRead(t *testing.T) {
	isolatePG(t)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var ef endpointFlags
	ef.register(fs)
	err := parseArgs(fs, []string{
		"-allow-foreign", "-url", "http://dispatch.example:8080", "-listener", "http://listener.example:9120",
		"-nats", "nats://nats.example:4222", "-nats-mon", "http://nats.example:8222", "-pprof", "http://dispatch.example:6061",
		"-pg", "postgres://postgres@db.example:5432/dispatch",
		"-token-file", credentialFileAt(t, "dispatch-token", "cluster-agent-token\n"),
		"-listener-token-file", credentialFileAt(t, "listener-token", "cluster-listener-token \t\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := resolveEndpoints(ef, nil)
	if err != nil {
		t.Fatalf("a foreign run given its tokens as files was refused: %v", err)
	}
	if e.token != "cluster-agent-token" || e.listenerToken != "cluster-listener-token" {
		t.Fatalf("token %q and listener token %q: want each its file's content, trailing white space trimmed", e.token, e.listenerToken)
	}
}

// TestAPostgresPasswordOnTheCommandLineIsRefused: -pg keeps its value form for a connection
// string that sets no password -- the local stack's own URL has none (stack.sh) -- and a value
// that sets one anywhere pgx's own parser finds it is refused by every command that takes -pg,
// before its first connection, naming -pg-file, with nothing of the value in the refusal.
func TestAPostgresPasswordOnTheCommandLineIsRefused(t *testing.T) {
	isolatePG(t)
	const frag = "pw-frag"
	pgOnce := func(pg string) []string { return []string{"-pg", pg} }
	withPassword := [][]string{
		pgOnce("postgres://postgres:" + frag + "1@db.example:5432/dispatch"),
		pgOnce("postgresql://db.example/dispatch?sslmode=disable&password=" + frag + "2"),
		// pgx decodes a parameter's name as it decodes its value.
		pgOnce("postgres://db.example/dispatch?pass%77ord=" + frag + "3"),
		// pgx ends the userinfo at the first @ before any /, so this is a password pgx sends,
		// where net/url reads a fragment from the # on and finds no user at all.
		pgOnce("postgres://u:#" + frag + "4@db.example/dispatch"),
		pgOnce("postgres://db.example/dispatch?sslpassword=" + frag + "5"),
		pgOnce("host=db.example user=postgres password=" + frag + "6"),
		// Every -pg given is on the command line, not the last alone, which is the one a
		// StringVar keeps.
		{"-pg", "postgres://postgres:" + frag + "7@db.example/dispatch", "-pg", "postgres://postgres@db.example/dispatch"},
		// A value pgx cannot parse: pgx's own refusal of it quotes it, password included.
		pgOnce("host=db.example user=postgres password = '" + frag + "8' sslmode=invalid"),
		// pgx's keyword grammar takes a vertical tab as white space around the =, as it takes a
		// tab or a space.
		pgOnce("host=db.example user=postgres password\v=" + frag + "9"),
		pgOnce("host=db.example user=postgres sslpassword\v=" + frag + "10"),
		// After a positional argument, or "--", where the flag package stops, a -pg is judged all
		// the same (parseArgs).
		{"stray", "-pg", "postgres://postgres:" + frag + "11@db.example/dispatch"},
		{"--", "--pg=host=db.example password=" + frag + "12"},
	}
	withoutPassword := []string{
		"postgres://postgres@127.0.0.1:15432/dispatch?sslmode=disable",
		"postgres://db1.example,db2.example:5433/dispatch?application_name=loadgen&target_session_attrs=read-write",
		"postgresql://u@[::1]:5432/dispatch?ssl=true",
	}
	tokenFiles := []string{
		"-token-file", credentialFileAt(t, "dispatch-token", "cluster-agent-token\n"),
		"-listener-token-file", credentialFileAt(t, "listener-token", "cluster-listener-token\n"),
	}
	// Each command that takes -pg, through what it does before its first connection. A value
	// with no password gets past the refusal looked for here, to the next check of the command
	// or to no error at all.
	commands := map[string]func(pg []string) error{
		"run": func(pg []string) error {
			fs := flag.NewFlagSet("run", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			var ef endpointFlags
			ef.register(fs)
			args := append([]string{
				"-allow-foreign", "-url", "http://dispatch.example:8080", "-listener", "http://listener.example:9120",
				"-nats", "nats://nats.example:4222", "-nats-mon", "http://nats.example:8222", "-pprof", "http://dispatch.example:6061",
			}, tokenFiles...)
			if err := parseArgs(fs, append(args, pg...)); err != nil {
				return err
			}
			_, err := resolveEndpoints(ef, nil)
			return err
		},
		"seed-finish": func(pg []string) error {
			_, err := seedFinishResolve(t, pg...)
			return err
		},
		"loop-seed-check": func(pg []string) error {
			_, err := loopSeedCheckResolve(t, append([]string{"-plan", "loopseed.plan.json"}, pg...)...)
			return err
		},
		"loop-seed-export": func(pg []string) error {
			_, err := loopSeedExportResolve(t, append([]string{"-plan", "loopseed.plan.json"}, pg...)...)
			return err
		},
		"pg-statements": func(pg []string) error {
			return runPGStatements(append([]string{"-step", t.TempDir(), "-interval", "0"}, pg...))
		},
		"pg-size": func(pg []string) error {
			return runPGSize(append([]string{"-step", t.TempDir(), "-phase", "neither"}, pg...))
		},
	}
	for command, resolve := range commands {
		for _, pg := range withPassword {
			err := resolve(pg)
			var password *pgPasswordError
			if !errors.As(err, &password) {
				t.Errorf("loadgen %s %q: error %v, want the password refused", command, pg, err)
				continue
			}
			if msg := err.Error(); !strings.Contains(msg, "-pg-file") || strings.Contains(msg, frag) {
				t.Errorf("loadgen %s %q: refusal %q, want one naming -pg-file and holding nothing of the value", command, pg, msg)
			}
		}
		for _, pg := range withoutPassword {
			var password *pgPasswordError
			if err := resolve(pgOnce(pg)); errors.As(err, &password) {
				t.Errorf("loadgen %s -pg %q: refused %v, want a URL with no password taken", command, pg, err)
			}
		}
	}
}
