package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// mainChildEnv makes this test binary the command rather than a test run: the command line, split
// on spaces, that TestMain runs main over (TestARefusedCommandLineIsWrittenOnceAndExits2).
const mainChildEnv = "EVALBENCH_TEST_MAIN_ARGS"

// TestMain runs the package's tests, or, where mainChildEnv is set, the command itself.
func TestMain(m *testing.M) {
	if args, child := os.LookupEnv(mainChildEnv); child {
		os.Args = append([]string{"evalbench"}, strings.Fields(args)...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestTheCodeFlagIsOnByDefaultAndOffByCodeFalse: the generator writes INFRA's code survival
// results unless -code=false says it must not, which a load into a database of a base that holds
// the eval registry but not the code evals needs (job.sh's base run); -base and -one-project write
// none whatever -code says, so INFRA-2916's runs, which name only them, do not move.
func TestTheCodeFlagIsOnByDefaultAndOffByCodeFalse(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		args []string
		code bool
	}{
		{"no flag", nil, true},
		{"-code", []string{"-code"}, true},
		{"-code=true", []string{"-code=true"}, true},
		{"-code=false", []string{"-code=false"}, false},
		{"-base and the default", []string{"-base"}, false},
		{"-one-project and the default", []string{"-one-project"}, false},
		{"-base and -code=false", []string{"-base", "-code=false"}, false},
		{"-code=false and a scale", []string{"-code=false", "-scale", "0.05"}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := parseConfig(append([]string{"-database", "postgres://unused"}, row.args...), io.Discard)
			if err != nil {
				t.Fatalf("parse %q: %v", row.args, err)
			}
			if got := cfg.spec.WritesCode(); got != row.code {
				t.Errorf("%q: the load writes the code survival results: %t, want %t", row.args, got, row.code)
			}
		})
	}
}

// TestACodeFlagThatIsNoBooleanIsRefused: -code reads a boolean, and a value that is none is
// refused with the flag's name, where a load that guessed would write the results or leave them out
// by chance.
func TestACodeFlagThatIsNoBooleanIsRefused(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	_, err := parseConfig([]string{"-database", "postgres://unused", "-code=maybe"}, &stderr)
	if err == nil {
		t.Fatal("-code=maybe was accepted")
	}
	if !strings.Contains(stderr.String(), "-code") {
		t.Errorf("the refusal %q does not name -code", stderr.String())
	}
}

// TestACommandLineKeepsItsOtherDefaults: the flags the bench passes before -code keep their
// defaults and their values, with -now read as the moment the history ends, and a command line with
// no database is the usage.
func TestACommandLineKeepsItsOtherDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := parseConfig([]string{"-database", "postgres://unused", "-built-by", "abc", "-now", "2026-11-13T00:00:00Z"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.spec.Seed != 2916 || cfg.spec.Scale != 1 || cfg.spec.TenTimes || cfg.spec.Base || cfg.spec.OneProject || cfg.spec.BuiltBy != "abc" {
		t.Errorf("the defaults moved: %+v", cfg.spec)
	}
	if want := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC); !cfg.spec.Now.Equal(want) {
		t.Errorf("-now read as %s, want %s", cfg.spec.Now, want)
	}
	if cfg.day.Runs != 36 || cfg.day.Jobs != 45 || cfg.day.DaysAgo != 0 {
		t.Errorf("the failure day's defaults moved: %+v", cfg.day)
	}
	if _, err := parseConfig(nil, io.Discard); !errors.Is(err, errUsage) {
		t.Errorf("a command line with no database answered %v, want the usage", err)
	}
	if _, err := parseConfig([]string{"-database", "postgres://unused", "-now", "yesterday"}, io.Discard); err == nil || errors.Is(err, errUsage) || errors.Is(err, flag.ErrHelp) {
		t.Errorf("-now yesterday answered %v, want a refusal that names it", err)
	}
}

// TestARefusedCommandLineIsWrittenOnceAndExits2: the command refuses a flag that does not parse and
// a -now that is no moment with exit 2 and the refusal on stderr once, the flag package's own line
// for a flag, where printing the parse's error again would say it twice.
func TestARefusedCommandLineIsWrittenOnceAndExits2(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		args, refusal string
	}{
		{"-database postgres://unused -code=maybe", `invalid boolean value "maybe" for -code`},
		{"-database postgres://unused -now yesterday", `read -now "yesterday" as a moment`},
	} {
		command := exec.Command(os.Args[0])
		command.Env = append(os.Environ(), mainChildEnv+"="+row.args)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		err := command.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Errorf("%q exited %v, want 2\n%s", row.args, err, stderr.String())
		}
		if count := strings.Count(stderr.String(), row.refusal); count != 1 {
			t.Errorf("%q wrote %q %d times, want once:\n%s", row.args, row.refusal, count, stderr.String())
		}
	}
}
