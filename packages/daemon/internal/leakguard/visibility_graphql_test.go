package leakguard

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// included is a GraphQL response as `gh api --include` prints it: the status line, the headers,
// with remaining points left and graphQLReset as the reset, then extra headers, a blank line and
// body.
func included(status string, remaining int, body string, extra ...string) string {
	headers := []string{status, "Content-Type: application/json; charset=utf-8", "X-Ratelimit-Limit: 12500",
		fmt.Sprintf("X-Ratelimit-Remaining: %d", remaining), fmt.Sprintf("X-Ratelimit-Reset: %d", graphQLReset.Unix()),
		"X-Ratelimit-Resource: graphql"}
	return strings.Join(append(headers, extra...), "\r\n") + "\r\n\r\n" + body
}

// Of what GraphQL says, PUBLIC, PRIVATE and INTERNAL for the repository asked about alone are an
// answer (graphQLAnswer). A rate limit of GraphQL's own, read from the error response alone, is a
// *rateLimitError, which sends the read on to ls-remote: errors every one of which is RATE_LIMITED
// or names the secondary rate limit, and what internal/githubrest reads as a limit (a 429; a 403
// with no points left, a Retry-After, or a message naming the secondary limit or GitHub's abuse
// detection mechanism). Everything else refuses, saying what GraphQL said. A wrong private or
// internal would skip the rule pass for a public repository, and a wrong public would widen what
// the shared check's fresh read of another remote counts as held, so none of these is read as an
// answer.
func TestVisibilityTakesOnlyAnAnswerGraphQLGives(t *testing.T) {
	const (
		ok         = "HTTP/2.0 200 OK"
		notFound   = `{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository with the name '` + theRepo + `'."}`
		rateLimit  = `{"type":"RATE_LIMITED","message":"API rate limit already exceeded for installation ID 1."}`
		forbidden  = `{"type":"FORBIDDEN","message":"Resource not accessible by integration"}`
		secondary  = "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."
		abuse      = "You have triggered an abuse detection mechanism. Please wait a few minutes before you try again."
		ghRefusal  = "gh: Resource not accessible by integration"
		ghNotFound = "gh: Could not resolve to a Repository with the name '" + theRepo + "'."
	)
	beside := func(visibility string, errs ...string) string {
		return `{"data":{"repository":{"nameWithOwner":"` + theRepo + `","visibility":"` + visibility + `"}},"errors":[` + strings.Join(errs, ",") + `]}`
	}
	for _, tc := range []struct {
		name    string
		exit    int
		printed string
		stderr  string
		// want is the answer; limit, where want is empty, is a refusal for a rate limit; fails is
		// what any other refusal says.
		want  Visibility
		limit bool
		fails string
	}{
		{name: "PUBLIC", printed: included(ok, 12221, answerBody(theRepo, "PUBLIC")), want: Public},
		{name: "PRIVATE", printed: included(ok, 12221, answerBody(theRepo, "PRIVATE")), want: Private},
		{name: "INTERNAL", printed: included(ok, 12221, answerBody(theRepo, "INTERNAL")), want: Internal},
		{name: "the repository in other capitals", printed: included(ok, 12221, answerBody("Invented-Org/Invented-Repo", "PRIVATE")), want: Private},
		{name: "a warning on gh's standard error", printed: included(ok, 12221, answerBody(theRepo, "PRIVATE")), stderr: "warning: invented", want: Private},

		{name: "errors all RATE_LIMITED", exit: 1, printed: included(ok, 0, `{"errors":[`+rateLimit+`]}`), limit: true},
		{name: "the status 403 with no points left", exit: 1, printed: included("HTTP/2.0 403 Forbidden", 0, `{"message":"API rate limit exceeded"}`), limit: true},
		{name: "the status 403 with no points left and a Retry-After", exit: 1, printed: included("HTTP/2.0 403 Forbidden", 0, `{"message":"invented"}`, "Retry-After: 60"), limit: true},
		{name: "the status 429 with no points left, naming the secondary limit", exit: 1, printed: included("HTTP/2.0 429 Too Many Requests", 0, `{"message":"`+secondary+`"}`), limit: true},
		{name: "the status 429 with no points left", exit: 1, printed: included("HTTP/2.0 429 Too Many Requests", 0, `{"message":"API rate limit exceeded"}`), limit: true},
		{name: "the status 429 with points left and nothing else", exit: 1, printed: included("HTTP/2.0 429 Too Many Requests", 12221, `{"message":"invented"}`), limit: true},
		{name: "the status 403 with a Retry-After", exit: 1, printed: included("HTTP/2.0 403 Forbidden", 12221, `{"message":"invented"}`, "Retry-After: 60"), limit: true},
		{name: "the status 429 with a Retry-After", exit: 1, printed: included("HTTP/2.0 429 Too Many Requests", 12221, `{"message":"invented"}`, "Retry-After: 60"), limit: true},
		{name: "the status 403 naming the secondary limit", exit: 1, printed: included("HTTP/2.0 403 Forbidden", 12221, `{"message":"`+secondary+`"}`), limit: true},
		{name: "the status 403 naming the abuse detection mechanism", exit: 1, printed: included("HTTP/2.0 403 Forbidden", 12221, `{"message":"`+abuse+`"}`), limit: true},
		{name: "the secondary limit inside errors, in a 403 with a Retry-After", exit: 1, printed: included("HTTP/2.0 403 Forbidden", 12221, `{"errors":[{"message":"`+secondary+`"}]}`, "Retry-After: 60"), limit: true},
		{name: "the secondary limit inside errors, in a 200", exit: 1, printed: included(ok, 12221, `{"errors":[{"message":"`+secondary+`"}]}`), limit: true},
		{name: "the abuse detection mechanism inside errors, in a 200", exit: 1, printed: included(ok, 12221, `{"errors":[{"message":"`+abuse+`"}]}`), limit: true},
		{name: "RATE_LIMITED and the secondary limit", exit: 1, printed: included(ok, 0, `{"errors":[`+rateLimit+`,{"message":"`+secondary+`"}]}`), limit: true},

		{name: "public in lower case", printed: included(ok, 12221, answerBody(theRepo, "public")), fails: `"public" as the visibility`},
		{name: "private in lower case", printed: included(ok, 12221, answerBody(theRepo, "private")), fails: `"private" as the visibility`},
		{name: "internal in lower case", printed: included(ok, 12221, answerBody(theRepo, "internal")), fails: `"internal" as the visibility`},
		{name: "an empty visibility", printed: included(ok, 12221, answerBody(theRepo, "")), fails: `"" as the visibility`},
		{name: "a visibility GitHub does not define", printed: included(ok, 12221, answerBody(theRepo, "SECRET")), fails: `"SECRET" as the visibility`},
		{
			name:    "another repository, as GitHub answers a renamed one's old name",
			printed: included(ok, 12221, answerBody("invented-org/renamed-repo", "PRIVATE")),
			fails:   `GraphQL answered for the repository "invented-org/renamed-repo", not ` + theRepo,
		},
		{name: "no repository name", printed: included(ok, 12221, `{"data":{"repository":{"visibility":"PRIVATE"}}}`), fails: `GraphQL answered for the repository "", not ` + theRepo},
		{name: "NOT_FOUND", exit: 1, printed: included(ok, 12221, `{"data":{"repository":null},"errors":[`+notFound+`]}`), stderr: ghNotFound, fails: "NOT_FOUND: Could not resolve to a Repository"},
		{name: "NOT_FOUND then RATE_LIMITED", exit: 1, printed: included(ok, 0, `{"data":{"repository":null},"errors":[`+notFound+`,`+rateLimit+`]}`), stderr: ghNotFound, fails: "NOT_FOUND: Could not resolve to a Repository"},
		{name: "RATE_LIMITED then NOT_FOUND", exit: 1, printed: included(ok, 0, `{"data":{"repository":null},"errors":[`+rateLimit+`,`+notFound+`]}`), stderr: ghNotFound, fails: "NOT_FOUND: Could not resolve to a Repository"},
		{name: "NOT_FOUND in a 403 with no points left", exit: 1, printed: included("HTTP/2.0 403 Forbidden", 0, `{"data":{"repository":null},"errors":[`+notFound+`]}`), stderr: ghNotFound, fails: "NOT_FOUND: Could not resolve to a Repository"},
		{name: "an error beside a public visibility", exit: 1, printed: included(ok, 12221, beside("PUBLIC", forbidden)), stderr: ghRefusal, fails: "FORBIDDEN: Resource not accessible by integration"},
		{name: "an error beside a private visibility", exit: 1, printed: included(ok, 12221, beside("PRIVATE", forbidden)), stderr: ghRefusal, fails: "FORBIDDEN: Resource not accessible by integration"},
		{name: "an error beside an internal visibility", exit: 1, printed: included(ok, 12221, beside("INTERNAL", forbidden)), stderr: ghRefusal, fails: "FORBIDDEN: Resource not accessible by integration"},
		{name: "an error beside a private visibility, exit 0", printed: included(ok, 12221, beside("PRIVATE", forbidden)), fails: "FORBIDDEN: Resource not accessible by integration"},
		{name: "an error beside an internal visibility, exit 0", printed: included(ok, 12221, beside("INTERNAL", forbidden)), fails: "FORBIDDEN: Resource not accessible by integration"},
		{name: "an error with no type", exit: 1, printed: included(ok, 12221, `{"errors":[{"message":"Something went wrong"}]}`), fails: "GraphQL answered " + theRepo + " with Something went wrong"},
		{name: "the secondary limit's words only on gh's standard error", exit: 1, printed: included(ok, 12221, `{"data":{"repository":null},"errors":[`+notFound+`]}`), stderr: "gh: " + secondary, fails: "NOT_FOUND: Could not resolve to a Repository"},
		{name: "no repository", printed: included(ok, 12221, `{"data":{"repository":null}}`), fails: "GraphQL answered no repository"},
		{name: "no data", printed: included(ok, 12221, `{"data":null}`), fails: "GraphQL answered no repository"},
		{name: "a second document after the answer", printed: included(ok, 12221, answerBody(theRepo, "PUBLIC")+answerBody(theRepo, "PRIVATE")), fails: "without one whole GraphQL answer"},
		{name: "bytes after the answer", printed: included(ok, 12221, answerBody(theRepo, "PRIVATE")+"invented"), fails: "without one whole GraphQL answer"},
		{name: "a body that is not JSON", exit: 1, printed: included("HTTP/2.0 502 Bad Gateway", 12221, "<html>bad gateway</html>"), stderr: "gh: HTTP 502", fails: "without one whole GraphQL answer"},
		{name: "the status 500 with a private body, exit 0", printed: included("HTTP/2.0 500 Internal Server Error", 12221, answerBody(theRepo, "PRIVATE")), fails: "answered the status 500"},
		{name: "a refused credential", exit: 1, printed: included("HTTP/2.0 401 Unauthorized", 12221, `{"message":"Bad credentials","status":"401"}`), stderr: "gh: Bad credentials (HTTP 401)", fails: "answered the status 401"},
		{name: "a valid body with exit 1", exit: 1, printed: included(ok, 12221, answerBody(theRepo, "PRIVATE")), fails: "exited 1"},
		{name: "no status line, a private body and exit 0", printed: answerBody(theRepo, "PRIVATE"), fails: "printed no status line this reads"},
		{name: "a malformed status line, a private body and exit 0", printed: included("HTTP/2.0 OK", 12221, answerBody(theRepo, "PRIVATE")), fails: "printed no status line this reads"},
		{name: "a status line with no code, a private body and exit 0", printed: included("HTTP/2.0", 12221, answerBody(theRepo, "PRIVATE")), fails: "printed no status line this reads"},
		{name: "a gh that cannot reach GitHub", exit: 1, stderr: "error connecting to api.github.com", fails: "error connecting to api.github.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			visibility, err := graphQLAnswer(tc.exit, tc.printed, tc.stderr, theRepo)
			var limited *rateLimitError
			switch {
			case tc.want != "":
				if err != nil || visibility != tc.want {
					t.Fatalf("graphQLAnswer = %q, %v; want %s", visibility, err, tc.want)
				}
			case tc.limit:
				if !errors.As(err, &limited) || visibility != "" {
					t.Fatalf("graphQLAnswer = %q, %v; want a refusal for a rate limit", visibility, err)
				}
			default:
				if err == nil || visibility != "" || errors.As(err, &limited) {
					t.Fatalf("graphQLAnswer = %q, %v; want a refusal that is no rate limit", visibility, err)
				}
				if !strings.Contains(err.Error(), tc.fails) {
					t.Errorf("the refusal %q does not say %q", err, tc.fails)
				}
				if strings.Contains(err.Error(), "with : ") {
					t.Errorf("the refusal %q names an error with no type as an empty one", err)
				}
			}
		})
	}
}

// The GraphQL read runs gh and reads what it printed with graphQLAnswer, standard output apart
// from standard error, as the guard's own exact argv with the variables naming the repository and
// keeping gh's output plain. Its answer stands; GraphQL's own rate limit goes on to ls-remote;
// anything else refuses the call, naming the spent core limit, its reset and what GraphQL said,
// and runs no git.
func TestVisibilityReadsGraphQLThroughGh(t *testing.T) {
	now := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	for _, tc := range []struct {
		name    string
		graphql func(record string) string
		// want is the answer, empty where the call is refused; source where the answer came from;
		// fails what a refusal says of GraphQL.
		want   Visibility
		source VisibilitySource
		fails  string
	}{
		{name: "an answer", graphql: func(r string) string { return graphQLAnswers(r, "PRIVATE") }, want: Private, source: VisibilityFromGraphQL},
		{
			name: "an answer beside a warning on standard error",
			graphql: func(r string) string {
				return graphQLRead(r, "HTTP/2.0 200 OK", 12221, answerBody(theRepo, "INTERNAL"), "warning: invented", 0)
			},
			want:   Internal,
			source: VisibilityFromGraphQL,
		},
		{name: "GraphQL's own rate limit", graphql: graphQLRateLimited, want: Public, source: VisibilityFromLsRemote},
		{name: "NOT_FOUND", graphql: graphQLNotFound, fails: "NOT_FOUND: Could not resolve to a Repository"},
		{
			name: "a gh that cannot reach GitHub",
			graphql: func(r string) string {
				return "echo read >> " + shQuote(r) + "\n" + printsToStderr("error connecting to api.github.com") + "exit 1\n"
			},
			fails: "error connecting to api.github.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, records := t.TempDir(), t.TempDir()
			graphql, record := filepath.Join(records, "graphql"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", tc.graphql(graphql))
			writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
			var sources []VisibilitySource
			visibility, err := New(Options{
				Clock:        &visibilityTestClock{now: now},
				OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
				Env:          append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
				Visibility: func(context.Context, string) (Visibility, error) {
					return "", &rateLimitError{reset: reset}
				},
			}).Visibility(bound(t), theRepo)
			if got := countLines(t, graphql); got != 1 {
				t.Errorf("gh's GraphQL read ran %d times, want once", got)
			}
			if queries := readCalls(t, graphql); len(queries) == 1 && queries[0]["argv"] != "" {
				if want := strings.Join(VisibilityGraphQLArgs(theRepo), " "); queries[0]["argv"] != want {
					t.Errorf("gh's GraphQL read ran as\n%s\nwant\n%s", queries[0]["argv"], want)
				}
				for key, want := range map[string]string{"read": theRepo, "host": "github.com", "nocolor": "1", "clicolor": "0"} {
					if got := queries[0][key]; got != want {
						t.Errorf("gh's GraphQL read ran with %s=%q, want %q", key, got, want)
					}
				}
			}
			gitRuns := 0
			if tc.source == VisibilityFromLsRemote {
				gitRuns = 1
			}
			if got := len(readCalls(t, record)); got != gitRuns {
				t.Errorf("git ran %d times, want %d", got, gitRuns)
			}
			if tc.want != "" {
				if err != nil || visibility != tc.want {
					t.Fatalf("read %q, %v; want %s", visibility, err, tc.want)
				}
				if want := []VisibilitySource{tc.source}; !slices.Equal(sources, want) {
					t.Errorf("the guard reported the sources %q, want %q", sources, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("read %q, want the call refused", visibility)
			}
			for _, want := range []string{
				"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
				"GraphQL could not answer: ",
				tc.fails,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the failure %q does not say %q", err, want)
				}
			}
			if len(sources) != 0 {
				t.Errorf("the guard reported the sources %q for a read that failed, want none", sources)
			}
		})
	}
}

// A caller's CLICOLOR_FORCE=1 does not reach the guard's gh reads, which would colour their
// output past reading: the REST read and the GraphQL read each run with NO_COLOR=1 and
// CLICOLOR_FORCE=0, and the call still answers.
func TestVisibilityKeepsGhsOutputPlain(t *testing.T) {
	now := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)
	bin, records := t.TempDir(), t.TempDir()
	rest, reads, graphql := filepath.Join(records, "rest"), filepath.Join(records, "reads"), filepath.Join(records, "graphql")
	writeProgram(t, bin, "gh", ghStandIn(
		`printf 'nocolor=%s\tclicolor=%s\n' "${NO_COLOR-unset}" "${CLICOLOR_FORCE-unset}" >> `+shQuote(rest)+"\n"+spentRateLimit(now.Add(30*time.Minute), reads),
		graphQLAnswers(graphql, "PUBLIC")))
	visibility, err := New(Options{
		Clock: &visibilityTestClock{now: now},
		Env:   append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir(), "CLICOLOR_FORCE=1"),
	}).Visibility(bound(t), theRepo)
	if err != nil || visibility != Public {
		t.Fatalf("read %q, %v; want public", visibility, err)
	}
	for name, calls := range map[string][]map[string]string{"REST": readCalls(t, rest), "GraphQL": readCalls(t, graphql)} {
		if len(calls) != 1 {
			t.Fatalf("gh's %s read ran %d times, want once", name, len(calls))
		}
		if calls[0]["nocolor"] != "1" || calls[0]["clicolor"] != "0" {
			t.Errorf("gh's %s read ran with NO_COLOR=%q and CLICOLOR_FORCE=%q, want 1 and 0", name, calls[0]["nocolor"], calls[0]["clicolor"])
		}
	}
}

// GitHub's secondary rate limit is read with ls-remote, which can answer only public, and never
// with GraphQL, whose requests count against the same secondary limit. A 403 whose message names
// the secondary limit, "secondary rate limit" or "secondary rate-limit", is that limit whatever
// X-RateLimit-Remaining says, as octokit's throttling plugin reads those words. The guard waits
// only for a Retry-After the wait bound and the caller's bound have room for, and then makes the
// REST read again. Any other secondary limit goes to ls-remote at once, which makes no API
// request. So the guard waits for no reset: not where requests are left, since GitHub sends
// X-RateLimit-Remaining and X-RateLimit-Reset for the core limit on every response and that reset
// then says nothing of the secondary limit, and not where none are left, a reset a minute away
// included, although GitHub's documentation asks a caller to wait for it before its next API
// request.
func TestVisibilityReadsASecondaryRateLimitWithLsRemote(t *testing.T) {
	now := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)
	far := fmt.Sprintf("'X-Ratelimit-Remaining: 21' 'X-Ratelimit-Reset: %d' ", now.Add(30*time.Minute).Unix())
	spent := func(reset time.Time) string {
		return fmt.Sprintf("'X-Ratelimit-Remaining: 0' 'X-Ratelimit-Reset: %d' ", reset.Unix())
	}
	// limitNamed answers the REST read with a 403 whose message says limit, as "secondary rate
	// limit" or "secondary rate-limit", with headers beside it.
	limitNamed := func(limit string) func(headers string) func(reads string) string {
		return func(headers string) func(reads string) string {
			return func(reads string) string {
				return "echo read >> " + shQuote(reads) + "\n" +
					"printf '%s\\r\\n' 'HTTP/2.0 403 Forbidden' 'Content-Type: application/json; charset=utf-8' " + headers + "''\n" +
					`printf '%s\n' '{"message":"You have exceeded a ` + limit + `. Please wait a few minutes before you try again."}'` + "\n" +
					printsToStderr("gh: You have exceeded a "+limit+". (HTTP 403)") + "exit 1\n"
			}
		}
	}
	secondary, hyphenated := limitNamed("secondary rate limit"), limitNamed("secondary rate-limit")
	for _, tc := range []struct {
		name string
		rest func(reads string) string
		git  string
		// room is whether the caller's bound has room for a wait (boundPastAWait) or not (bound).
		room bool
		// want is the answer, empty where the call is refused; reads how many REST reads ran, waits
		// the waits made, gitRuns how many times git ran, and fails what a refusal says.
		want    Visibility
		reads   int
		waits   []time.Duration
		gitRuns int
		fails   string
	}{
		{
			name: "requests left and a reset, as GitHub sends its secondary limit, and no Retry-After",
			rest: secondary(far), git: answersTheRefs, room: true, want: Public, reads: 1, gitRuns: 1,
		},
		{
			name: "a message saying secondary rate-limit, with a hyphen, beside requests left and a reset",
			rest: hyphenated(far), git: answersTheRefs, room: true, want: Public, reads: 1, gitRuns: 1,
		},
		{
			name: "a Retry-After beside a reset 30 minutes away is waited alone",
			rest: secondary("'Retry-After: 60' " + far), git: answersTheRefs, room: true,
			want: Public, reads: 2, waits: []time.Duration{time.Minute}, gitRuns: 1,
		},
		{
			name: "no requests left beside a Retry-After is the secondary limit",
			rest: secondary("'X-Ratelimit-Remaining: 0' 'Retry-After: 60' "), git: answersTheRefs, room: true,
			want: Public, reads: 2, waits: []time.Duration{time.Minute}, gitRuns: 1,
		},
		{
			name: "a Retry-After the caller's bound has no room for",
			rest: secondary("'Retry-After: 60' "), git: answersTheRefs, want: Public, reads: 1, gitRuns: 1,
		},
		{
			name: "a Retry-After past the wait bound",
			rest: secondary("'Retry-After: 600' "), git: answersTheRefs, room: true, want: Public, reads: 1, gitRuns: 1,
		},
		{
			name: "no Retry-After, and ls-remote fails", rest: secondary(""), git: cannotConnect, room: true, reads: 1, gitRuns: 1,
			fails: "GitHub's secondary rate limit refused the read, and git ls-remote could not answer",
		},
		{
			name: "no requests left and no Retry-After, beside the message, is the secondary limit",
			rest: secondary(spent(now.Add(30 * time.Minute))), git: answersTheRefs, room: true, want: Public, reads: 1, gitRuns: 1,
		},
		{
			name: "no requests left and a reset a minute away, beside the message, is not waited for",
			rest: secondary(spent(now.Add(time.Minute))), git: answersTheRefs, room: true, want: Public, reads: 1, gitRuns: 1,
		},
		{
			name: "a message saying secondary rate-limit, with a hyphen, beside no requests left and a reset a minute away",
			rest: hyphenated(spent(now.Add(time.Minute))), git: answersTheRefs, room: true, want: Public, reads: 1, gitRuns: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, records := t.TempDir(), t.TempDir()
			reads, graphql, record := filepath.Join(records, "reads"), filepath.Join(records, "graphql"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", ghStandIn(tc.rest(reads), graphQLAnswers(graphql, "PRIVATE")))
			writeProgram(t, bin, "git", recordsItsCall(record)+tc.git)
			clock := &visibilityTestClock{now: now}
			ctx := bound(t)
			if tc.room {
				ctx = boundPastAWait(t)
			}
			visibility, err := New(Options{
				Clock: clock,
				Env:   append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
			}).Visibility(ctx, theRepo)
			if tc.want != "" && (err != nil || visibility != tc.want) {
				t.Errorf("read %q, %v; want %s", visibility, err, tc.want)
			}
			if tc.want == "" && (err == nil || !strings.Contains(err.Error(), tc.fails)) {
				t.Errorf("read %q, %v; want the call refused saying %q", visibility, err, tc.fails)
			}
			if got := countLines(t, reads); got != tc.reads {
				t.Errorf("gh's REST read ran %d times, want %d", got, tc.reads)
			}
			if !slices.Equal(clock.waits, tc.waits) {
				t.Errorf("the waits are %v, want %v", clock.waits, tc.waits)
			}
			if got := len(readCalls(t, graphql)); got != 0 {
				t.Errorf("gh's GraphQL read ran %d times, want none", got)
			}
			if got := len(readCalls(t, record)); got != tc.gitRuns {
				t.Errorf("git ran %d times, want %d", got, tc.gitRuns)
			}
		})
	}
}

// A wait for a near reset that the caller's bound would cut short is not made: the guard reads the
// visibility through GraphQL at once, as it does for a caller whose bound is shorter than the wait
// and the reads after it.
func TestVisibilityWaitsForNoResetPastTheCallersBound(t *testing.T) {
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(time.Minute)
	bin, graphql := t.TempDir(), filepath.Join(t.TempDir(), "graphql")
	writeProgram(t, bin, "gh", graphQLAnswers(graphql, "PRIVATE"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	var reads int
	visibility, err := New(Options{
		Clock: clock,
		Env:   append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
		Visibility: func(context.Context, string) (Visibility, error) {
			reads++
			return "", &rateLimitError{reset: reset}
		},
	}).Visibility(ctx, theRepo)
	if err != nil || visibility != Private {
		t.Fatalf("read %q, %v; want private from GraphQL", visibility, err)
	}
	if reads != 1 || len(clock.waits) != 0 {
		t.Errorf("the REST read ran %d times and the waits are %v, want one read and no wait", reads, clock.waits)
	}
	if got := len(readCalls(t, graphql)); got != 1 {
		t.Errorf("gh's GraphQL read ran %d times, want once", got)
	}
}

// rateLimitFrom reads the REST read's error response alone, with internal/githubrest's model: a
// 429, and a 403 with no requests left, a Retry-After, or a message naming the secondary limit or
// GitHub's abuse detection mechanism, are a limit. It is the secondary limit where the message
// names it, whatever X-RateLimit-Remaining says, or where the response has a Retry-After or leaves
// requests, and the guard keeps only its Retry-After, never its X-RateLimit-Reset; otherwise it
// is the installation's core limit, with its reset. The secondary limit's words in gh's own line,
// a 404 naming it, and a 403 that says none of these are no limit.
func TestRateLimitFromReadsTheErrorResponseAlone(t *testing.T) {
	reset := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	response := func(status string, headers []string, body, ghLine string) string {
		return strings.Join(append([]string{status, "Content-Type: application/json; charset=utf-8"}, headers...), "\r\n") +
			"\r\n\r\n" + body + "\n" + ghLine + "\n"
	}
	spent := []string{"X-Ratelimit-Remaining: 0", fmt.Sprintf("X-Ratelimit-Reset: %d", reset.Unix())}
	left := []string{"X-Ratelimit-Remaining: 21", fmt.Sprintf("X-Ratelimit-Reset: %d", reset.Unix())}
	const (
		secondary = `{"message":"You have exceeded a secondary rate limit."}`
		abuse     = `{"message":"You have triggered an abuse detection mechanism."}`
	)
	for _, tc := range []struct {
		name      string
		printed   string
		limit     bool
		secondary bool
		reset     time.Time
		retry     time.Duration
	}{
		{name: "403 with no requests left", printed: response("HTTP/2.0 403 Forbidden", spent, `{"message":"API rate limit exceeded"}`, "gh: API rate limit exceeded (HTTP 403)"), limit: true, reset: reset},
		{name: "429 with no requests left", printed: response("HTTP/2.0 429 Too Many Requests", spent, `{"message":"API rate limit exceeded"}`, ""), limit: true, reset: reset},
		{name: "403 with no requests left and the secondary message is the secondary limit", printed: response("HTTP/2.0 403 Forbidden", spent, secondary, ""), limit: true, secondary: true},
		{name: "403 with no requests left and the abuse message is the secondary limit", printed: response("HTTP/2.0 403 Forbidden", spent, abuse, ""), limit: true, secondary: true},
		{name: "403 with a Retry-After", printed: response("HTTP/2.0 403 Forbidden", []string{"Retry-After: 60"}, `{"message":"invented"}`, ""), limit: true, secondary: true, retry: time.Minute},
		{name: "429 with a Retry-After", printed: response("HTTP/2.0 429 Too Many Requests", []string{"Retry-After: 60"}, `{"message":"invented"}`, ""), limit: true, secondary: true, retry: time.Minute},
		{name: "403 naming the secondary limit", printed: response("HTTP/2.0 403 Forbidden", nil, secondary, ""), limit: true, secondary: true},
		{name: "403 naming the abuse detection mechanism", printed: response("HTTP/2.0 403 Forbidden", nil, abuse, ""), limit: true, secondary: true},
		{name: "a 429 with nothing else", printed: response("HTTP/2.0 429 Too Many Requests", nil, `{"message":"invented"}`, ""), limit: true, secondary: true},
		{
			name:    "403 with requests left and a reset, naming the secondary limit, as GitHub sends it",
			printed: response("HTTP/2.0 403 Forbidden", left, secondary, ""),
			limit:   true, secondary: true,
		},
		{
			name:    "403 with no requests left, a Retry-After and the secondary message",
			printed: response("HTTP/2.0 403 Forbidden", []string{"X-Ratelimit-Remaining: 0", "Retry-After: 60"}, secondary, ""),
			limit:   true, secondary: true, retry: time.Minute,
		},
		{
			name:    "429 with no requests left, a reset and a Retry-After",
			printed: response("HTTP/2.0 429 Too Many Requests", append([]string{"Retry-After: 60"}, spent...), `{"message":"invented"}`, ""),
			limit:   true, secondary: true, retry: time.Minute,
		},
		{name: "404 naming the secondary limit", printed: response("HTTP/2.0 404 Not Found", nil, secondary, "")},
		{name: "the secondary limit's words in gh's line only", printed: response("HTTP/2.0 403 Forbidden", nil, `{"message":"invented"}`, "gh: You have exceeded a secondary rate limit. (HTTP 403)")},
		{name: "a 403 with requests left and nothing else", printed: response("HTTP/2.0 403 Forbidden", left, `{"message":"Resource not accessible by integration"}`, "")},
		{name: "an answer", printed: response("HTTP/2.0 200 OK", []string{"X-Ratelimit-Remaining: 4999"}, "public", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limited := rateLimitFrom(tc.printed)
			if (limited != nil) != tc.limit {
				t.Fatalf("rateLimitFrom = %v, want a limit: %v", limited, tc.limit)
			}
			if limited != nil && (limited.secondary != tc.secondary || !limited.reset.Equal(tc.reset) || limited.retry != tc.retry) {
				t.Errorf("rateLimitFrom = secondary %v reset %s retry %s, want %v, %s and %s",
					limited.secondary, limited.reset, limited.retry, tc.secondary, tc.reset, tc.retry)
			}
		})
	}
}

// When GitHub refuses the visibility read for a spent core limit whose reset is 30 minutes away,
// past the wait bound, the guard reads the visibility through GitHub's GraphQL API, and only where
// GraphQL refuses the read for a rate limit of its own through git's ls-remote. GraphQL's public
// is scanned, and its private and internal are not; ls-remote's public is scanned. A GraphQL that
// could not resolve the repository, and an ls-remote that fails, git's own line for a server that
// asked for a credential included, refuse the push, naming the spent limit and its reset. In
// every case gh's REST read and its GraphQL read run once each, as the guard's own exact argv
// with the variable naming the repository, and nothing is waited for. git runs only after
// GraphQL's rate limit, once, with the credential helper, the extra headers and
// http.proactiveAuth emptied after the package's pins and before ls-remote, and with
// GIT_TERMINAL_PROMPT=0.
func TestVisibilityWithoutRESTDecidesThePush(t *testing.T) {
	now := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	scanned := func(source VisibilitySource) func(*testing.T, error, []VisibilitySource) {
		return func(t *testing.T, err error, sources []VisibilitySource) {
			t.Helper()
			if refused := refusal(t, err); refused.By != byRules {
				t.Errorf("the push was refused by %q, want the rules: a public destination is scanned", refused.By)
			}
			if want := []VisibilitySource{source}; !slices.Equal(sources, want) {
				t.Errorf("the guard reported the sources %q, want %q", sources, want)
			}
		}
	}
	unscanned := func(t *testing.T, err error, sources []VisibilitySource) {
		t.Helper()
		if err != nil {
			t.Fatalf("the push to a private or internal destination: %v; want it passed, unscanned", err)
		}
		if want := []VisibilitySource{VisibilityFromGraphQL}; !slices.Equal(sources, want) {
			t.Errorf("the guard reported the sources %q, want %q", sources, want)
		}
	}
	refusedSaying := func(wants ...string) func(*testing.T, error, []VisibilitySource) {
		return func(t *testing.T, err error, sources []VisibilitySource) {
			t.Helper()
			says(t, setupFailure(t, err), append([]string{
				"the visibility of " + theRepo + " could not be read",
				"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
			}, wants...)...)
			if len(sources) != 0 {
				t.Errorf("the guard reported the sources %q for a read that failed, want none", sources)
			}
		}
	}
	answers := func(visibility string) func(string) string {
		return func(record string) string { return graphQLAnswers(record, visibility) }
	}
	for _, tc := range []struct {
		name string
		// graphql is what the stand-in gh's GraphQL read does, given the file it records its call
		// in, and git what the stand-in git does after it records its call.
		graphql func(record string) string
		git     string
		// gitRuns is how many times git runs: once where GraphQL is rate-limited, else never.
		gitRuns int
		// check reads the push's answer and the sources the guard reported.
		check func(t *testing.T, err error, sources []VisibilitySource)
	}{
		{name: "GraphQL answers public", graphql: answers("PUBLIC"), git: answersTheRefs, check: scanned(VisibilityFromGraphQL)},
		{name: "GraphQL answers private", graphql: answers("PRIVATE"), git: answersTheRefs, check: unscanned},
		{name: "GraphQL answers internal", graphql: answers("INTERNAL"), git: answersTheRefs, check: unscanned},
		{
			name:    "GraphQL could not resolve the repository",
			graphql: graphQLNotFound,
			git:     answersTheRefs,
			check:   refusedSaying("GraphQL could not answer", "NOT_FOUND"),
		},
		{
			name:    "GraphQL is rate-limited and ls-remote answers",
			graphql: graphQLRateLimited,
			git:     answersTheRefs,
			gitRuns: 1,
			check:   scanned(VisibilityFromLsRemote),
		},
		{
			name:    "GraphQL is rate-limited and GitHub asks ls-remote for a credential",
			graphql: graphQLRateLimited,
			git:     asksForACredential,
			gitRuns: 1,
			check:   refusedSaying("GraphQL's rate limit is spent too", "could not read Username"),
		},
		{
			name:    "GraphQL is rate-limited and ls-remote fails another way",
			graphql: graphQLRateLimited,
			git:     cannotConnect,
			gitRuns: 1,
			check:   refusedSaying("GraphQL's rate limit is spent too", "Could not resolve host"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, records := t.TempDir(), t.TempDir()
			reads, graphql, record := filepath.Join(records, "reads"), filepath.Join(records, "graphql"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", ghStandIn(spentRateLimit(reset, reads), tc.graphql(graphql)))
			writeProgram(t, bin, "git", recordsItsCall(record)+tc.git)
			clock := &visibilityTestClock{now: now}
			var sources []VisibilitySource
			guard := New(Options{
				Check:        leakguardtest.Write(t, t.TempDir(), passes),
				Clock:        clock,
				OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
				Env:          append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
			})
			// A ref the push deletes, named for a host the rules refuse: its name is the push's one
			// record, so a scan refuses the push and no scan passes it, and no git reads a commit.
			err := guard.Push(bound(t), PushRequest{
				Dir:     t.TempDir(),
				Remote:  "origin",
				URL:     theURL,
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: noSHA, RemoteRef: "refs/heads/legion/" + theHost, RemoteSHA: noSHA}},
			})
			tc.check(t, err, sources)
			if got := countLines(t, reads); got != 1 {
				t.Errorf("gh's REST read ran %d times, want once", got)
			}
			if len(clock.waits) != 0 {
				t.Errorf("the waits are %v, want none: the reset is past the wait bound", clock.waits)
			}
			queries := readCalls(t, graphql)
			if len(queries) != 1 {
				t.Fatalf("gh's GraphQL read ran %d times, want once", len(queries))
			}
			if want := strings.Join(VisibilityGraphQLArgs(theRepo), " "); queries[0]["argv"] != want {
				t.Errorf("gh's GraphQL read ran as\n%s\nwant\n%s", queries[0]["argv"], want)
			}
			if queries[0]["read"] != theRepo || queries[0]["host"] != "github.com" {
				t.Errorf("gh's GraphQL read ran with %s=%q and GH_HOST=%q, want %q and github.com", VisibilityReadVar, queries[0]["read"], queries[0]["host"], theRepo)
			}
			calls := readCalls(t, record)
			if len(calls) != tc.gitRuns {
				t.Fatalf("git ran %d times, want %d", len(calls), tc.gitRuns)
			}
			if tc.gitRuns == 0 {
				return
			}
			if want := strings.Join(gitPins, " ") + " " + theResets + " ls-remote " + theURL + " HEAD"; calls[0]["argv"] != want {
				t.Errorf("git ran as\n%s\nwant\n%s", calls[0]["argv"], want)
			}
			if calls[0]["prompt"] != "0" {
				t.Errorf("git ran with GIT_TERMINAL_PROMPT=%s, want 0", calls[0]["prompt"])
			}
		})
	}
}

// A GraphQL read that the caller's bound stops refuses the call at once, naming the spent limit,
// its reset and gh's stop, and does not go on to ls-remote: no bound is left for it. The bound
// ends only once the stand-in gh has started, so it is a running gh the bound stops.
func TestVisibilityRefusesAGraphQLReadTheCallersBoundStops(t *testing.T) {
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(30 * time.Minute)
	bin, records := t.TempDir(), t.TempDir()
	started, record := filepath.Join(records, "started"), filepath.Join(records, "git")
	writeProgram(t, bin, "gh", ": > "+shQuote(started)+"\nexec sleep 86400\n")
	writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
	deadline, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	ctx := &deadlineOnceStarted{Context: deadline, done: make(chan struct{})}
	whenWritten(bound(t), started, func() {
		<-deadline.Done()
		close(ctx.done)
	})
	guard := New(Options{
		Clock: clock,
		Env:   append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
		Visibility: func(context.Context, string) (Visibility, error) {
			return "", &rateLimitError{reset: reset}
		},
	})
	var err error
	answered(t, "the visibility read whose gh never answers", func() { _, err = guard.Visibility(ctx, theRepo) })
	if err == nil {
		t.Fatal("the visibility read passed while gh never answered")
	}
	for _, want := range []string{
		"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
		"GraphQL could not answer: gh ran ",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure %q does not say %q", err, want)
		}
	}
	if got := stopBound(t, err); got > 2 {
		t.Errorf("gh was stopped by a bound of %d s, want the caller's deadline of 2 s", got)
	}
	if calls := readCalls(t, record); len(calls) != 0 {
		t.Errorf("git ran %d times, want none: the caller's bound is spent", len(calls))
	}
}

// An answer read without REST is kept by the rules a gh answer is (Guard.keepVisibility): public
// for the pod's life, so the next call reads neither gh nor git, and private and internal not at
// all, so the next call reads them again. That holds for GraphQL's answers and for ls-remote's
// public. Each call builds a guard of its own, as every call site does.
func TestVisibilityKeepsAnAnswerWithoutRESTAsItKeepsGhs(t *testing.T) {
	now := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	answers := func(visibility string) func(string) string {
		return func(record string) string { return graphQLAnswers(record, visibility) }
	}
	for _, tc := range []struct {
		name    string
		graphql func(record string) string
		want    Visibility
		// reads is how many times each of gh's two reads runs over the two calls, and gitRuns how
		// many times git does.
		reads   int
		gitRuns int
		sources []VisibilitySource
	}{
		{
			name:    "GraphQL's public",
			graphql: answers("PUBLIC"),
			want:    Public,
			reads:   1,
			sources: []VisibilitySource{VisibilityFromGraphQL, VisibilityFromCache},
		},
		{
			name:    "GraphQL's private",
			graphql: answers("PRIVATE"),
			want:    Private,
			reads:   2,
			sources: []VisibilitySource{VisibilityFromGraphQL, VisibilityFromGraphQL},
		},
		{
			name:    "GraphQL's internal",
			graphql: answers("INTERNAL"),
			want:    Internal,
			reads:   2,
			sources: []VisibilitySource{VisibilityFromGraphQL, VisibilityFromGraphQL},
		},
		{
			name:    "ls-remote's public",
			graphql: graphQLRateLimited,
			want:    Public,
			reads:   1,
			gitRuns: 1,
			sources: []VisibilitySource{VisibilityFromLsRemote, VisibilityFromCache},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, state, records := t.TempDir(), t.TempDir(), t.TempDir()
			reads, graphql, record := filepath.Join(records, "reads"), filepath.Join(records, "graphql"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", ghStandIn(spentRateLimit(reset, reads), tc.graphql(graphql)))
			writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
			var sources []VisibilitySource
			for range 2 {
				visibility, err := New(Options{
					Clock:        &visibilityTestClock{now: now},
					OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
					Env:          append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+state),
				}).Visibility(bound(t), theRepo)
				if err != nil || visibility != tc.want {
					t.Fatalf("read %q, %v; want %s", visibility, err, tc.want)
				}
			}
			if !slices.Equal(sources, tc.sources) {
				t.Errorf("the guards reported the sources %q, want %q", sources, tc.sources)
			}
			if got := countLines(t, reads); got != tc.reads {
				t.Errorf("gh's REST read ran %d times, want %d", got, tc.reads)
			}
			if got := len(readCalls(t, graphql)); got != tc.reads {
				t.Errorf("gh's GraphQL read ran %d times, want %d", got, tc.reads)
			}
			if got := len(readCalls(t, record)); got != tc.gitRuns {
				t.Errorf("git ran %d times, want %d", got, tc.gitRuns)
			}
		})
	}
}
