package ghtool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// standInSource is the Kubernetes runtime's reader of the shared check as these tests serve it
// (LeakCheckSource): body, packed, and its manifest digest, at any tag, with each tag it was read
// at recorded.
type standInSource struct {
	body     []byte
	manifest string
	tags     []string
}

func (s *standInSource) Get(_ context.Context, tag string) (leakguard.Served, error) {
	s.tags = append(s.tags, tag)
	return leakguard.Served{Tag: tag, Body: s.body, ManifestSHA256: s.manifest}, nil
}

// servedSource is the reader serving check, the leak-check directory's files by name, packed and
// digested as the daemon's reader packs GitHub's tarball of the check's repository
// (leakguard.PackTarballManifest).
func servedSource(t *testing.T, check map[string]leakguardtest.Member) *standInSource {
	t.Helper()
	body, manifest, err := leakguard.PackTarballManifest(bytes.NewReader(leakguardtest.GitHubTarball(t, check)))
	if err != nil {
		t.Fatalf("pack the served check: %v", err)
	}
	return &standInSource{body: body, manifest: manifest}
}

// refusingSource is a reader whose every read fails with err, as GitHub's refusal of the read
// reaches installLeakCheck.
type refusingSource struct{ err error }

func (s refusingSource) Get(context.Context, string) (leakguard.Served, error) {
	return leakguard.Served{}, s.err
}

// servedCheck is the leak-check directory the daemon serves: a launcher that answers the install's
// one `--help` run as the real check does, counting it in runs (leakguardtest.AnswersHelp), and
// clears every call it is handed after, recording it in record (checkPasses, recordsIn), beside
// the three other files of its manifest (leakguard.ManifestFiles).
func servedCheck(runs, record string) map[string]leakguardtest.Member {
	return checkFiles("#!/bin/sh\nif [ \"$1\" = --help ]; then\n" + leakguardtest.AnswersHelp(runs) + "fi\n" + recordsIn(record, checkPasses))
}

// checkFiles is a leak-check directory whose launcher is launcher, beside leakguardtest's stand-in
// program, README and must-pass lines, the four files of its manifest.
func checkFiles(launcher string) map[string]leakguardtest.Member {
	return map[string]leakguardtest.Member{
		leakguardtest.CheckName:    {Body: launcher, Mode: 0o755},
		leakguardtest.ProgramName:  {Body: leakguardtest.Program, Mode: 0o644},
		leakguardtest.ReadmeName:   {Body: leakguardtest.Readme, Mode: 0o644},
		leakguardtest.MustPassName: {Body: leakguardtest.MustPass, Mode: 0o644},
	}
}

// installedAt is a Kubernetes daemon's check once an install landed at check (LeakCheck.Install).
func installedAt(check string) *LeakCheck {
	return &LeakCheck{tag: "leak-check-v18", installed: check}
}

// daemonEnviron is a daemon's own environment as the guard's children start from it (Host.Environ)
// with extra after it, and with no extra one holding none of the three places the guard resolves a
// check from: no PUBLIC_LEAK_CHECK, a PATH of the system's own directories, and a configuration
// home with no pointer file. That is a Kubernetes daemon's, whose image carries the check's pin and
// not the check. It carries a state home of its own.
func daemonEnviron(t *testing.T, extra ...string) []string {
	t.Helper()
	home := t.TempDir()
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_STATE_HOME=" + t.TempDir()}
	if check, err := leakguard.New(leakguard.Options{Env: env}).Resolve(); err == nil {
		t.Fatalf("the daemon's environment resolves the check %s, where it must hold none", check)
	}
	return append(env, extra...)
}

// A Kubernetes daemon's gh tool runs the check the daemon installed for it, read through the
// runtime's own reader at the pin and of the deployment's manifest digest (installLeakCheck): a
// call the installed check clears runs gh, with no setup failure, in an environment that holds no
// check of its own, the guard told that digest and holding the install to it. The install is a
// pod's launcher's: one `--help` run of the served check, the tag recorded, at
// <state_dir>/leak-check. Resolved in that environment, as a tmux daemon's check is
// (MachineLeakCheck), there is none, and every call is refused with the setup failure naming it,
// gh never started.
func TestACallRunsUnderTheCheckAKubernetesDaemonInstalled(t *testing.T) {
	const tag = "leak-check-v18"
	runs, record := filepath.Join(t.TempDir(), "help-runs"), filepath.Join(t.TempDir(), "leak-check-record")
	source := servedSource(t, servedCheck(runs, record))
	stateDir := t.TempDir()
	check, err := installLeakCheck(context.Background(), source, tag, source.manifest, stateDir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("installLeakCheck: %v", err)
	}
	if want := filepath.Join(stateDir, "leak-check", leakguardtest.CheckName); check != want || !slices.Equal(source.tags, []string{tag}) {
		t.Fatalf("the install is at %s after reads at %q, want %s from one read at %s", check, source.tags, want, tag)
	}
	if verified, _ := os.ReadFile(runs); string(verified) != "help\n" {
		t.Fatalf("the install ran the served check %q, want its one --help run", verified)
	}
	argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}

	t.Run("the installed check clears the call and gh runs", func(t *testing.T) {
		f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
		// The installed check records in the file written into it, which is what f.checked reads.
		f.record = record
		f.answer.host.LeakCheck, f.answer.host.Environ = installedAt(check), daemonEnviron(t)
		f.answer.host.LeakCheckManifestSHA256 = source.manifest
		got := f.run(1, argv...)
		if !got.Ran || got.Exit != 0 || got.Output != "posted\n" {
			t.Fatalf("gh %q = %+v; want gh's answer, exit 0, and no setup failure", argv, got)
		}
		if ran := f.invocations(false); len(ran) != 1 {
			t.Fatalf("gh %q ran gh %d times: %+v", argv, len(ran), ran)
		}
		if checked := f.checked(); !strings.Contains(checked, checkedCall(argv[:4])) {
			t.Errorf("the installed check recorded %q, want the call", checked)
		}
	})

	t.Run("the daemon's environment resolves none", func(t *testing.T) {
		f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
		environ := daemonEnviron(t)
		f.answer.host.LeakCheck, f.answer.host.Environ = MachineLeakCheck{Environ: environ}, environ
		got := f.run(1, argv...)
		const want = "LEAK CHECK SETUP ERROR: the gh call: the shared public leak check could not be found"
		if got.Ran || got.Exit != -1 || !strings.HasPrefix(got.Output, "refused: ") || !strings.Contains(got.Output, want) {
			t.Fatalf("gh %q = %+v; want a refusal containing %q, gh never run", argv, got, want)
		}
		if ran := f.invocations(false); len(ran) != 0 {
			t.Errorf("gh ran for a call no check cleared: %+v", ran)
		}
	})
}

// A failure of the install itself, after the reader served the check, is the cause sentence a
// pod's start is refused with, which names the failure: here a served check whose one `--help`
// run names none of its shapes, which leakguard.InstallServed refuses. A read that fails is the
// reader's own error as it came, with no sentence of the install around it.
func TestAFailedInstallIsItsCauseSentenceAndAFailedReadIsTheReadsOwn(t *testing.T) {
	const tag = "leak-check-v18"
	silent := servedSource(t, checkFiles("#!/bin/sh\nexit 2\n"))
	_, err := installLeakCheck(context.Background(), silent, tag, silent.manifest, t.TempDir(), slog.New(slog.DiscardHandler))
	if want := leakguard.RefusalPrefix + "the served check did not answer: its --help names no check-public-text shape"; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("the install of a check whose --help fails = %v, want the cause sentence %q…", err, want)
	}

	read := errors.New(leakguard.RefusalPrefix + "GitHub answered the read of example-org/deployment at " + tag + " with HTTP 502")
	_, err = installLeakCheck(context.Background(), refusingSource{err: read}, tag, silent.manifest, t.TempDir(), slog.New(slog.DiscardHandler))
	if !errors.Is(err, read) || err.Error() != read.Error() {
		t.Fatalf("an install whose read failed = %v, want the read's own error %q", err, read)
	}
}

// A call that comes before a Kubernetes daemon's install lands is refused, gh never started, not
// even for its help, with the setup failure naming the pin and why the last try of the install
// failed (LeakCheck.Install, which holds the try's error and returns it), recorded and logged as
// the guard's own refusals are. The first call after a try lands, in the same answer, runs under
// the check that try installed.
func TestACallBeforeTheInstallLandsIsRefusedAndTheNextAfterItRuns(t *testing.T) {
	const tag = "leak-check-v18"
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
	source := servedSource(t, servedCheck(filepath.Join(t.TempDir(), "help-runs"), f.record))
	pending := NewLeakCheck(tag, source.manifest)
	stateDir := t.TempDir()
	cause := errors.New(leakguard.RefusalPrefix + "GitHub answered the read of example-org/deployment at " + tag + " with HTTP 502")
	if err := pending.Install(context.Background(), refusingSource{err: cause}, stateDir, slog.New(slog.DiscardHandler)); !errors.Is(err, cause) {
		t.Fatalf("a try whose read failed = %v, want the read's own error", err)
	}
	f.answer.host.LeakCheck, f.answer.host.Environ = pending, daemonEnviron(t)
	argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}

	got := f.run(1, argv...)
	want := "refused: LEAK CHECK SETUP ERROR: the gh call: the shared public leak check at " + tag + ", the pin " + leakguard.TagVariable +
		" names, is not installed yet, and the daemon tries the install again until it is: " + cause.Error()
	if got.Ran || got.Exit != -1 || got.Output != want {
		t.Fatalf("gh %q before the install = %+v; want the refusal %q, gh never run", argv, got, want)
	}
	if ran := f.invocations(true); len(ran) != 0 {
		t.Errorf("gh ran for a call refused before the install landed: %+v", ran)
	}
	row, recorded := f.calls.row(question, 1)
	if !recorded || row.Result == nil || *row.Result != got.Output || row.Exit != nil || row.Owner != "" {
		t.Errorf("the refused call is recorded as %+v (%t); want its refusal, no exit and no owner", row, recorded)
	}
	if lines := f.logLines("answers: gh call refused by the leak guard"); len(lines) != 1 || !strings.Contains(fmt.Sprint(lines[0]["why"]), "is not installed yet") {
		t.Errorf("the refusal logged %v; want one line naming the pending install", lines)
	}

	if err := pending.Install(context.Background(), source, stateDir, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("a try the reader serves: %v", err)
	}
	if got := f.run(2, argv...); !got.Ran || got.Exit != 0 || got.Output != "posted\n" {
		t.Fatalf("gh %q after the install = %+v; want gh's answer, exit 0", argv, got)
	}
	if checked := f.checked(); !strings.Contains(checked, checkedCall(argv[:4])) {
		t.Errorf("the installed check recorded %q, want the call after the install", checked)
	}
	if check, err := pending.Check(); err != nil || check != filepath.Join(stateDir, "leak-check", leakguardtest.CheckName) {
		t.Errorf("the check after the install = %q, %v; want the copy under %s", check, err, stateDir)
	}
}

// Once the daemon stops trying the install (LeakCheck.Stop), over a cause no wait changes, every
// call is refused, gh never started, with the setup failure that says the check could not be
// installed and is tried again only at the daemon's next boot, the cause underneath: never the
// pending refusal, which says the daemon tries again.
func TestACallAfterTheInstallStoppedIsRefusedSayingTheNextBootTriesAgain(t *testing.T) {
	const tag = "leak-check-v18"
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
	stopped := NewLeakCheck(tag, strings.Repeat("5e", 32))
	cause := errors.New(leakguard.RefusalPrefix + "GitHub answered the read of example-org/deployment at " + tag + " with HTTP 404")
	if err := stopped.Install(context.Background(), refusingSource{err: cause}, t.TempDir(), slog.New(slog.DiscardHandler)); !errors.Is(err, cause) {
		t.Fatalf("a try whose read failed = %v, want the read's own error", err)
	}
	stopped.Stop(cause)
	f.answer.host.LeakCheck, f.answer.host.Environ = stopped, daemonEnviron(t)
	argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}
	want := "refused: LEAK CHECK SETUP ERROR: the gh call: the shared public leak check at " + tag + ", the pin " + leakguard.TagVariable +
		" names, could not be installed, and the daemon tries the install again only at its next boot: " + cause.Error()
	for seq := 1; seq <= 2; seq++ {
		if got := f.run(seq, argv...); got.Ran || got.Exit != -1 || got.Output != want {
			t.Fatalf("gh %q, call %d, after the install stopped = %+v; want the refusal %q, gh never run", argv, seq, got, want)
		}
	}
	if ran := f.invocations(true); len(ran) != 0 {
		t.Errorf("gh ran for a call refused after the install stopped: %+v", ran)
	}
}

// noCheck is a host's check that answers no path and no error, which no check of the daemon's
// answers (LeakChecker).
type noCheck struct{}

func (noCheck) Check() (string, error) { return "", nil }

// A call whose host answers an empty check, and no setup failure, is refused before the guard
// runs, with the setup failure naming the empty check, recorded and logged as the guard's own
// refusals are. Handed no check, the guard would resolve one of its own (leakguard's Resolve):
// here the check the pointer file under the daemon's configuration home names, where the tool
// runs no check but the one its host answers. gh never runs, and neither does that check.
func TestACallWhoseHostAnswersAnEmptyCheckIsRefusedBeforeTheGuardRuns(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
	configHome := t.TempDir()
	pointer := filepath.Join(configHome, "public-leak-check", "path")
	if err := os.MkdirAll(filepath.Dir(pointer), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pointer, []byte(f.path+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.answer.host.LeakCheck = noCheck{}
	f.answer.host.Environ = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + configHome, "XDG_STATE_HOME=" + t.TempDir()}
	argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}

	got := f.run(1, argv...)
	const why = "LEAK CHECK SETUP ERROR: the gh call: the daemon handed the guard no shared public leak check for this call, " +
		"and the gh tool runs no check but the one the daemon hands it: the check's path is empty"
	if got.Ran || got.Exit != -1 || got.Output != "refused: "+why {
		t.Fatalf("gh %q under a host that answers an empty check = %+v; want the refusal %q, gh never run", argv, got, "refused: "+why)
	}
	if ran := f.invocations(false); len(ran) != 0 {
		t.Errorf("gh ran for a call whose host answered an empty check: %+v", ran)
	}
	if checked := f.checked(); checked != "" {
		t.Errorf("the check the pointer file names recorded %q, want it never run", checked)
	}
	row, recorded := f.calls.row(question, 1)
	if !recorded || row.Result == nil || *row.Result != got.Output || row.Exit != nil || row.Owner != "" {
		t.Errorf("the refused call is recorded as %+v (%t); want its refusal, no exit and no owner", row, recorded)
	}
	if lines := f.logLines("answers: gh call refused by the leak guard"); len(lines) != 1 || lines[0]["why"] != why {
		t.Errorf("the refusal logged %v; want one line naming the empty check", lines)
	}
}

// A tmux daemon's gh tool resolves the machine's own check in the daemon's environment at each
// call (MachineLeakCheck), as the tmux runtime resolves a pane's at each launch: a call runs the
// check the machine's pointer file names when the call is made, the next call after the check was
// republished elsewhere runs the new copy, and a call made while the machine carries none is
// refused with the setup failure naming the missing check, gh never run.
func TestEachCallRunsTheMachinesCheckAsItIsWhenTheCallIsMade(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
	configHome := t.TempDir()
	pointer := filepath.Join(configHome, "public-leak-check", "path")
	point := func(check string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(pointer), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pointer, []byte(check+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	firstRecord, againRecord := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "again")
	first := leakguardtest.Write(t, t.TempDir(), recordsIn(firstRecord, checkPasses))
	republished := leakguardtest.Write(t, t.TempDir(), recordsIn(againRecord, checkPasses))
	environ := []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + configHome, "XDG_STATE_HOME=" + t.TempDir()}
	f.answer.host.LeakCheck, f.answer.host.Environ = MachineLeakCheck{Environ: environ}, environ
	argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}
	checked := func(record string) int {
		t.Helper()
		recorded, err := os.ReadFile(record)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return bytes.Count(recorded, []byte(checkedCall(argv[:4])))
	}

	point(first)
	if got := f.run(1, argv...); !got.Ran || got.Exit != 0 || checked(firstRecord) != 1 {
		t.Fatalf("the first call = %+v, checked %d times by the machine's check; want it run, checked once", got, checked(firstRecord))
	}
	point(republished)
	if got := f.run(2, argv...); !got.Ran || got.Exit != 0 || checked(againRecord) != 1 || checked(firstRecord) != 1 {
		t.Fatalf("the call after the check moved = %+v, checked %d times by the new copy and %d by the old; want it run under the new copy alone",
			got, checked(againRecord), checked(firstRecord))
	}
	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}
	got := f.run(3, argv...)
	const want = "LEAK CHECK SETUP ERROR: the gh call: the shared public leak check could not be found"
	if got.Ran || got.Exit != -1 || !strings.Contains(got.Output, want) {
		t.Fatalf("the call on a machine with no check = %+v; want a refusal containing %q", got, want)
	}
	if ran := f.invocations(false); len(ran) != 2 {
		t.Errorf("gh ran %d calls, want the two the machine's check cleared: %+v", len(ran), ran)
	}
}

// A tmux daemon's check is resolved in the daemon's own environment (Host.Environ), never under
// the environment the guard's children run in (Answer.guardEnv), whose PATH has gh's directory in
// front and which carries no PUBLIC_LEAK_CHECK: the check PUBLIC_LEAK_CHECK names in the daemon's
// environment, on no PATH entry, is the one the guard runs, and a public-leak-check.sh in gh's
// directory, on the guard's PATH alone, is never run.
func TestTheMachinesCheckIsResolvedInTheDaemonsEnvironmentAndNotTheGuards(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
	namedRecord, onGuardPathRecord := filepath.Join(t.TempDir(), "named"), filepath.Join(t.TempDir(), "on-guard-path")
	named := leakguardtest.Write(t, t.TempDir(), recordsIn(namedRecord, checkPasses))
	leakguardtest.Write(t, f.stand, recordsIn(onGuardPathRecord, checkPasses))
	environ := []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + t.TempDir(), "XDG_STATE_HOME=" + t.TempDir(),
		leakguard.CheckVar + "=" + named}
	f.answer.host.LeakCheck, f.answer.host.Environ = MachineLeakCheck{Environ: environ}, environ
	// Resolved in the guard's environment, the check would be the one in gh's directory.
	if resolved, err := leakguard.New(leakguard.Options{Env: f.answer.guardEnv(t.TempDir())}).Resolve(); err != nil || filepath.Dir(resolved) != f.stand {
		t.Fatalf("the guard's environment resolves %q, %v; want the check in gh's directory %s", resolved, err, f.stand)
	}
	argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}
	if got := f.run(1, argv...); !got.Ran || got.Exit != 0 || got.Output != "posted\n" {
		t.Fatalf("gh %q = %+v; want gh's answer, exit 0", argv, got)
	}
	checked := func(record string) int {
		t.Helper()
		recorded, err := os.ReadFile(record)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return bytes.Count(recorded, []byte(checkedCall(argv[:4])))
	}
	if byName, onGuardPath := checked(namedRecord), checked(onGuardPathRecord); byName != 1 || onGuardPath != 0 {
		t.Fatalf("the call was checked %d times by the check %s names and %d by the one on the guard's PATH; want once by the named one alone",
			byName, leakguard.CheckVar, onGuardPath)
	}
}

// A re-pin is a boot at another tag: its install replaces the copy of the tag before at the same
// path, logging the one line that says so, and the path reads back as the new tag's, of the
// deployment's manifest digest.
func TestAnInstallAtANewPinReplacesTheOneBefore(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "help-runs")
	source := servedSource(t, servedCheck(runs, filepath.Join(t.TempDir(), "leak-check-record")))
	stateDir := t.TempDir()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	first, err := installLeakCheck(context.Background(), source, "leak-check-v18", source.manifest, stateDir, log)
	if err != nil {
		t.Fatal(err)
	}
	second, err := installLeakCheck(context.Background(), source, "leak-check-v19", source.manifest, stateDir, log)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateDir, "leak-check")
	if second != first {
		t.Fatalf("the second install is at %s, want the first's path %s", second, first)
	}
	if check, err := leakguard.Installed([]string{leakguard.TagVariable + "=leak-check-v19", leakguard.CheckerVar + "=" + source.manifest}, dir); err != nil || check != second {
		t.Fatalf("the install at %s reads back as %q, %v; want %s, of leak-check-v19 and the digest %s", dir, check, err, second, source.manifest)
	}
	if want := "answers: the install of leak-check-v19 replaced the install of leak-check-v18 at " + dir; !strings.Contains(logs.String(), want) {
		t.Errorf("the re-pin logged %q, want the line %q", logs.String(), want)
	}
}

// The guard of every call is told the deployment's manifest digest as leakguard.CheckerVar where
// the deployment names a check (Host.LeakCheckManifestSHA256), once and with the setting's own
// value, and none where it names none, whatever the daemon's own environment carries under that
// name. Told it, the guard holds the check the host answers to it, as a pane's or a pod's guard
// holds its role's: under a check of another digest a call is refused, gh never run, with the
// setup failure naming the digest told and the check's own, where the same call runs under the
// check's own digest and under none.
func TestTheGuardIsHeldToTheDeploymentsDigestOnlyWhereItNamesOne(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
	// The fixture's check with the three other files of its manifest beside it, so it has one.
	leakguardtest.Companions(t, f.check, leakguardtest.Program)
	own, err := leakguard.Manifest(func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(f.check, name)) })
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("0", 64)
	f.answer.host.Environ = append(f.answer.host.Environ, leakguard.CheckerVar+"="+strings.Repeat("7c", 32))
	argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}
	for seq, tc := range []struct {
		name, manifest string
		told           []string
		runs           bool
	}{
		{name: "a deployment that names the check's own digest", manifest: own, told: []string{own}, runs: true},
		{name: "a deployment that names none", runs: true},
		{name: "a deployment that names another digest", manifest: other, told: []string{other}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.answer.host.LeakCheckManifestSHA256 = tc.manifest
			var told []string
			for _, pair := range f.answer.guardEnv(t.TempDir()) {
				if value, ok := strings.CutPrefix(pair, leakguard.CheckerVar+"="); ok {
					told = append(told, value)
				}
			}
			if !slices.Equal(told, tc.told) {
				t.Fatalf("the guard is told %s %q, want %q", leakguard.CheckerVar, told, tc.told)
			}
			ran := len(f.invocations(false))
			got := f.run(seq+1, argv...)
			if tc.runs {
				if !got.Ran || got.Exit != 0 || got.Output != "posted\n" {
					t.Fatalf("gh %q = %+v; want gh's answer, exit 0", argv, got)
				}
				return
			}
			const refused = "LEAK CHECK SETUP ERROR: the gh call: the shared public leak check found is not the one the deployment names: "
			if got.Ran || got.Exit != -1 || !strings.Contains(got.Output, refused+leakguard.CheckerVar+"="+other) ||
				!strings.Contains(got.Output, "has the manifest digest "+own) {
				t.Fatalf("gh %q = %+v; want the setup failure naming the digest told and the check's own, gh never run", argv, got)
			}
			if now := len(f.invocations(false)); now != ran {
				t.Errorf("gh ran %d calls for a call refused for its check's digest", now-ran)
			}
		})
	}
}

// Under the deployment's manifest digest the guard of a call runs its private copy of the check
// from the answer's own 0700 directory under the daemon's state directory (Answer.copies,
// leakguard.Options.PrivateCopyBase), never from leakguard.PrivateCopyBase or /tmp: a Kubernetes
// daemon's container holds the worker image's root-owned PrivateCopyBase, where no copy can be
// made. The fixture's check records the directory it ran from, and the copy is gone once the call
// answers.
func TestACallUnderTheDigestRunsTheCheckFromACopyUnderTheStateDirectory(t *testing.T) {
	f := newFixture(t, implementer, behaviour{Stdout: "posted\n"})
	ran := filepath.Join(t.TempDir(), "ran-from")
	f.leakCheck("echo \"$(dirname \"$(readlink -f \"$0\")\")\" >>'" + ran + "'\n" + checkPasses)
	leakguardtest.Companions(t, f.check, leakguardtest.Program)
	own, err := leakguard.Manifest(func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(f.check, name)) })
	if err != nil {
		t.Fatal(err)
	}
	f.answer.host.LeakCheckManifestSHA256 = own
	argv := []string{"pr", "comment", "5", "-R", "acme/widgets", "--body", "Done."}
	if got := f.run(1, argv...); !got.Ran || got.Exit != 0 || got.Output != "posted\n" {
		t.Fatalf("gh %q = %+v; want gh's answer, exit 0", argv, got)
	}
	recorded, err := os.ReadFile(ran)
	if err != nil {
		t.Fatalf("the check recorded no run: %v", err)
	}
	copies, err := filepath.EvalSymlinks(f.answer.copies)
	if err != nil {
		t.Fatal(err)
	}
	state, err := filepath.EvalSymlinks(f.host.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	from := strings.TrimSpace(string(recorded))
	if filepath.Dir(from) != copies || !strings.HasPrefix(copies, state+string(filepath.Separator)) {
		t.Fatalf("the check ran from %q, want one copy in %s, under the state directory %s", from, copies, state)
	}
	if info, err := os.Stat(copies); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the copies' directory %s is %v, %v; want a 0700 directory", copies, info, err)
	}
	if _, err := os.Lstat(from); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy %s is still there once the call answered (%v)", from, err)
	}
}
