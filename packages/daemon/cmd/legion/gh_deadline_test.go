package main

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// ghCallerDeadline takes LEGION_GH_DEADLINE where it is a base-10 count of Unix milliseconds with
// no sign and no space, later than now, and nothing else: an unset or empty variable, a word, a
// negative number, a sign, a space, a count past int64 and a time not later than now are all no
// deadline, as an unset one is.
func TestGhCallerDeadlineIsALaterTimeInUnixMilliseconds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	ahead := now.Add(8 * time.Second)
	at := func(when time.Time) string { return strconv.FormatInt(when.UnixMilli(), 10) }
	for _, tc := range []struct {
		name string
		env  map[string]string
		want time.Time
	}{
		{name: "unset", env: map[string]string{}},
		{name: "empty", env: map[string]string{ghDeadlineVar: ""}},
		{name: "a word", env: map[string]string{ghDeadlineVar: "abc"}},
		{name: "a negative number", env: map[string]string{ghDeadlineVar: "-1"}},
		{name: "a time a second past", env: map[string]string{ghDeadlineVar: at(now.Add(-time.Second))}},
		{name: "now itself", env: map[string]string{ghDeadlineVar: at(now)}},
		{name: "8 s ahead", env: map[string]string{ghDeadlineVar: at(ahead)}, want: ahead},
		{name: "8 s ahead after a space", env: map[string]string{ghDeadlineVar: " " + at(ahead)}},
		{name: "8 s ahead before a space", env: map[string]string{ghDeadlineVar: at(ahead) + " "}},
		{name: "8 s ahead with a plus sign", env: map[string]string{ghDeadlineVar: "+" + at(ahead)}},
		{name: "a count past int64", env: map[string]string{ghDeadlineVar: "99999999999999999999"}},
	} {
		got, ok := ghCallerDeadline(tc.env, now)
		if ok != !tc.want.IsZero() || !got.Equal(tc.want) {
			t.Errorf("ghCallerDeadline(%s) = %s, %t; want %s, %t", tc.name, got, ok, tc.want, !tc.want.IsZero())
		}
	}
}

// `legion gh` runs its guard under the deadline its caller gives in LEGION_GH_DEADLINE, so the
// guard's wait for a near reset of a spent core limit (leakguard's waitFits) is made only where it
// fits before that deadline. The stand-in gh answers the guard's REST visibility read with
// GitHub's refusal for a spent core limit and its GraphQL read with PRIVATE. Under a deadline 8 s
// ahead, with the reset 60 s away, the guard does not wait: it reads GraphQL once, at once, and the
// call runs, all well before the reset. With no variable, or one already past, or one that does not
// parse, the guard still waits for the reset, as it does for every call that gives no deadline,
// then reads REST again and, still refused, reads GraphQL. Those rows put the reset 5 s away, so
// the wait is seen in the second REST read and costs the row 5 s rather than a minute; the parse
// itself is TestGhCallerDeadlineIsALaterTimeInUnixMilliseconds's.
func TestLegionGhRunsItsGuardUnderTheCallersDeadline(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	const slug = "acme/widgets"
	later := func(lead time.Duration) func() string {
		return func() string { return strconv.FormatInt(time.Now().Add(lead).UnixMilli(), 10) }
	}
	for _, tc := range []struct {
		name string
		// deadline is LEGION_GH_DEADLINE when the call starts, unset where it is nil.
		deadline func() string
		// resetIn is how many seconds after its answer the stand-in's spent limit resets.
		resetIn int
		// waits is whether the guard waits for the reset: it then reads REST a second time.
		waits bool
	}{
		{name: "a deadline 8 s ahead", deadline: later(8 * time.Second), resetIn: 60},
		{name: "no deadline", resetIn: 5, waits: true},
		{name: "a deadline a second past", deadline: later(-time.Second), resetIn: 5, waits: true},
		{name: "a deadline that does not parse", deadline: func() string { return "abc" }, resetIn: 5, waits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			record := filepath.Join(t.TempDir(), "reads")
			stub := filepath.Join(t.TempDir(), "real-gh")
			script := ghStandInHelp(help) +
				"case \" $* \" in\n" +
				"\" api --hostname github.com graphql \"*)\n" +
				"  echo graphql >>" + shellWord(record) + "\n" +
				"  printf '%s\\r\\n' 'HTTP/2.0 200 OK' 'X-Ratelimit-Remaining: 12221' ''\n" +
				"  printf '%s' '{\"data\":{\"repository\":{\"nameWithOwner\":\"" + slug + "\",\"visibility\":\"PRIVATE\"}}}'\n" +
				"  exit 0;;\n" +
				"\" api --hostname github.com repos/" + slug + " \"*\" --jq .visibility \")\n" +
				"  echo rest >>" + shellWord(record) + "\n" +
				"  printf '%s\\r\\n' 'HTTP/2.0 403 Forbidden' 'X-Ratelimit-Remaining: 0' \"X-Ratelimit-Reset: $(( $(date +%s) + " + strconv.Itoa(tc.resetIn) + " ))\" ''\n" +
				"  printf '%s\\n' '{\"message\":\"API rate limit exceeded for installation ID 1.\"}'\n" +
				"  echo 'gh: API rate limit exceeded for installation ID 1. (HTTP 403)' >&2\n" +
				"  exit 1;;\n" +
				"esac\n" +
				"echo gh >>" + shellWord(record) + "\n"
			if err := execfile.Write(stub, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{
				"GH_CONFIG_DIR":        defaultDir,
				"LEGION_GH_OWNERS_DIR": root,
				"LEGION_GH_PATH":       stub,
				"PUBLIC_LEAK_CHECK":    leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
				leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
			}
			if tc.deadline != nil {
				env[ghDeadlineVar] = tc.deadline()
			}
			args := []string{"pr", "create", "-R", slug, "--title", "a title the rules pass"}
			started := time.Now()
			code, stdout, stderr := legionGh(t, env, args...)
			took := time.Since(started)
			if code != 0 || stdout != "" {
				t.Fatalf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and nothing on standard output", args, code, stdout, stderr)
			}
			reads := "rest\ngraphql\ngh\n"
			if tc.waits {
				reads = "rest\n" + reads
			}
			if got := readRecord(t, record); got != reads {
				t.Errorf("the stand-in gh recorded %q, want %q", got, reads)
			}
			if reset := time.Duration(tc.resetIn) * time.Second; !tc.waits && took >= reset {
				t.Errorf("the call took %s, want it done before the reset %s away, which it does not wait for", took.Round(time.Millisecond), reset)
			}
		})
	}
}
