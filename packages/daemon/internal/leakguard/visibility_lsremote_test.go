package leakguard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

// The read with no credential carries no credential of the pod's. Of the caller's GIT_ variables
// only the transport one reaches git, so neither the GIT_CONFIG_COUNT block a pod's git gets its
// credential helper from, nor GIT_CONFIG_PARAMETERS holding an extra Authorization header and
// http.proactiveAuth, nor a GIT_DIR whose configuration could hold either does. Neither the
// caller's HOME, whose .netrc libcurl would read, nor NETRC does: git's HOME is an empty
// directory, made for the read and removed after it. git reads no system or user configuration
// file, runs in / outside any repository, has the credential helper, the extra headers and
// http.proactiveAuth emptied in front of ls-remote, has an askpass that answers nothing, and
// prints its messages in English.
func TestLsRemoteReadsWithNoCredentialOfThePods(t *testing.T) {
	bin, record, home := t.TempDir(), filepath.Join(t.TempDir(), "record"), t.TempDir()
	writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
	netrc := "machine github.com\nlogin invented-user\npassword invented-password\n"
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(netrc), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := New(Options{Env: append(fixtureenv.Isolated(home),
		pathWith(bin),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_0=!invented-helper",
		"GIT_CONFIG_PARAMETERS='http.extraheader'='Authorization: Basic aW52ZW50ZWQ6aW52ZW50ZWQ=' 'http.proactiveauth'='basic'",
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_DIR="+t.TempDir(),
		"GIT_ALLOW_PROTOCOL=https",
		"NETRC="+filepath.Join(home, ".netrc"),
		"LC_ALL=fr_FR.UTF-8",
	)})
	visibility, err := guard.lsRemote(bound(t), githubOrigin, theRepo)
	if err != nil || visibility != Public {
		t.Fatalf("read %q, %v; want public", visibility, err)
	}
	calls := readCalls(t, record)
	if len(calls) != 1 {
		t.Fatalf("git ran %d times, want once", len(calls))
	}
	for key, want := range map[string]string{
		"prompt":     "0",
		"nosystem":   "1",
		"global":     "/dev/null",
		"count":      "unset",
		"key0":       "unset",
		"parameters": "unset",
		"gitdir":     "unset",
		"protocol":   "https",
		"lang":       "C",
		"netrc":      "unset",
		"homeholds":  "",
		"dir":        "/",
	} {
		if got := calls[0][key]; got != want {
			t.Errorf("git ran with %s=%q, want %q", key, got, want)
		}
	}
	gitHome := calls[0]["home"]
	if gitHome == "unset" || gitHome == "" || gitHome == home {
		t.Errorf("git ran with HOME=%q, want an empty directory of the read's own, not the caller's %s", gitHome, home)
	} else if _, err := os.Stat(gitHome); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("git's HOME %s is still there after the read (%v), want it removed", gitHome, err)
	}
	if resets := " " + theResets + " ls-remote " + theURL + " "; !strings.Contains(" "+calls[0]["argv"]+" ", resets) {
		t.Errorf("git ran as %q, want the credential helper, the extra headers and http.proactiveAuth emptied in front of ls-remote %s", calls[0]["argv"], theURL)
	}
	askpass := calls[0]["askpass"]
	if askpass == "unset" || askpass == "" {
		t.Fatalf("git ran with no askpass program: %q", askpass)
	}
	if status, _, err := guard.run(bound(t), child{argv: []string{askpass}}); err != nil || status == 0 {
		t.Errorf("the askpass program %s answered %d, %v; want a program that answers no credential", askpass, status, err)
	}
}

// The read with no credential reaches GitHub the way the caller's environment says to, and asks
// for no credential however the caller asks for one. The CA bundle git trusts (GIT_SSL_CAINFO,
// GIT_SSL_CAPATH) and libcurl's proxy (HTTPS_PROXY), none of which carries a credential, reach
// git as the caller set them. A caller's GIT_TERMINAL_PROMPT=1, and a GIT_ASKPASS that would
// print a credential, do not: git sees GIT_TERMINAL_PROMPT=0 and the guard's own askpass.
func TestLsRemoteKeepsTheCallersTransportButNotItsPrompts(t *testing.T) {
	bin, record, askpasses := t.TempDir(), filepath.Join(t.TempDir(), "record"), t.TempDir()
	writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
	writeProgram(t, askpasses, "answers", "echo invented-token\n")
	cainfo, capath := filepath.Join(t.TempDir(), "ca.pem"), t.TempDir()
	guard := New(Options{Env: append(fixtureenv.Isolated(t.TempDir()),
		pathWith(bin),
		"GIT_SSL_CAINFO="+cainfo,
		"GIT_SSL_CAPATH="+capath,
		"HTTPS_PROXY=http://proxy.invented:3128",
		"GIT_TERMINAL_PROMPT=1",
		"GIT_ASKPASS="+filepath.Join(askpasses, "answers"),
	)})
	visibility, err := guard.lsRemote(bound(t), githubOrigin, theRepo)
	if err != nil || visibility != Public {
		t.Fatalf("read %q, %v; want public", visibility, err)
	}
	calls := readCalls(t, record)
	if len(calls) != 1 {
		t.Fatalf("git ran %d times, want once", len(calls))
	}
	for key, want := range map[string]string{
		"cainfo":  cainfo,
		"capath":  capath,
		"proxy":   "http://proxy.invented:3128",
		"prompt":  "0",
		"askpass": guard.noAnswer,
	} {
		if got := calls[0][key]; got != want {
			t.Errorf("git ran with %s=%q, want %q", key, got, want)
		}
	}
}

// No netrc credential reaches the real git of the read with no credential, whether the .netrc is
// in the caller's home or NETRC names it (libcurl reads NETRC from curl 8.16; an older libcurl
// ignores it). A server of the test's own answers every request 401 with a challenge, as GitHub
// answers an anonymous read of a private repository, and both netrc files name that server's
// host. libcurl answers such a challenge with a netrc credential it finds by sending the request
// again with an Authorization header (measured by hand with git 2.47.3 and the caller's HOME);
// here the server sees no such header, and the read is no answer.
func TestLsRemoteSendsNoNetrcCredential(t *testing.T) {
	git := realGit(t)
	var mu sync.Mutex
	var requests, authorized int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		if r.Header.Get("Authorization") != "" {
			authorized++
		}
		mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="invented"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	host, _, _ := strings.Cut(strings.TrimPrefix(server.URL, "http://"), ":")
	netrc := []byte("machine " + host + "\nlogin invented-user\npassword invented-password\n")
	home, named := t.TempDir(), filepath.Join(t.TempDir(), "netrc")
	for _, path := range []string{filepath.Join(home, ".netrc"), named} {
		if err := os.WriteFile(path, netrc, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	guard := New(Options{Env: loopbackEnv(home, pathWith(filepath.Dir(git)), "NETRC="+named)})
	visibility, err := guard.lsRemote(bound(t), server.URL, "private/invented-repo")
	if err == nil {
		t.Fatalf("read %q, want no answer: a 401 is not one", visibility)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests == 0 {
		t.Fatal("git sent the server no request")
	}
	if authorized != 0 {
		t.Errorf("%d of git's %d requests carried an Authorization header: a netrc credential reached the read", authorized, requests)
	}
}

// The read with no credential ends at its own cap when git never answers, inside a caller's bound
// that would let it run longer, and the call is refused naming the spent limit and git's stop by
// a bound no longer than that cap. GraphQL's own limit is spent, which is what sends the read on
// to git.
func TestLsRemoteThatHangsEndsAtItsOwnBound(t *testing.T) {
	saved := lsRemoteBound
	lsRemoteBound = 2 * time.Second
	t.Cleanup(func() { lsRemoteBound = saved })
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(30 * time.Minute)
	bin := t.TempDir()
	writeProgram(t, bin, "gh", graphQLRateLimited(filepath.Join(t.TempDir(), "graphql")))
	writeProgram(t, bin, "git", "exec sleep 86400\n")
	guard := New(Options{
		Clock: clock,
		Env:   append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
		Visibility: func(context.Context, string) (Visibility, error) {
			return "", &rateLimitError{reset: reset}
		},
	})
	ctx := bound(t)
	var err error
	answered(t, "the visibility read whose git never answers", func() { _, err = guard.Visibility(ctx, theRepo) })
	if err == nil {
		t.Fatal("the visibility read passed while git never answered")
	}
	for _, want := range []string{
		"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
		"GraphQL's rate limit is spent too, and git ls-remote could not answer: git ran ",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure %q does not say %q", err, want)
		}
	}
	if got := stopBound(t, err); got > 2 {
		t.Errorf("git was stopped by a bound of %d s, want the read's own %s, not the caller's minute", got, lsRemoteBound)
	}
}

// The read with no credential also ends when the caller's context ends, before its own cap: a
// caller that cancels while git hangs, and one whose deadline comes before lsRemoteBound, each
// get the call back at once, refused naming the spent limit, its reset and git's stop. Nothing is
// kept, so the next call reads the visibility again. lsRemoteBound keeps its own two minutes
// here. Each caller's context ends only once the stand-in git has started, so it is a running git
// the end stops on a loaded machine too. GraphQL's own limit is spent, which is what sends the
// read on to git.
func TestLsRemoteStopsWithTheCallersContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		// caller is the caller's context, given the file the stand-in git writes before it
		// hangs.
		caller func(t *testing.T, started string) context.Context
		// stop checks what the failure says of git's stop.
		stop func(t *testing.T, err error)
	}{
		{
			name: "the caller cancels",
			caller: func(t *testing.T, started string) context.Context {
				ctx, cancel := context.WithCancel(bound(t))
				t.Cleanup(cancel)
				whenWritten(ctx, started, cancel)
				return ctx
			},
			stop: func(t *testing.T, err error) {
				if want := "and was stopped by a cancel"; !strings.Contains(err.Error(), want) {
					t.Errorf("the failure %q does not say %q", err, want)
				}
			},
		},
		{
			name: "the caller's deadline comes first",
			caller: func(t *testing.T, started string) context.Context {
				deadline, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				t.Cleanup(cancel)
				ctx := &deadlineOnceStarted{Context: deadline, done: make(chan struct{})}
				whenWritten(bound(t), started, func() {
					<-deadline.Done()
					close(ctx.done)
				})
				return ctx
			},
			stop: func(t *testing.T, err error) {
				if got := stopBound(t, err); got > 2 {
					t.Errorf("git was stopped by a bound of %d s, want the caller's deadline of 2 s, not the read's own %s", got, lsRemoteBound)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
			reset := clock.Now().Add(30 * time.Minute)
			bin, started := t.TempDir(), filepath.Join(t.TempDir(), "started")
			writeProgram(t, bin, "gh", graphQLRateLimited(filepath.Join(t.TempDir(), "graphql")))
			writeProgram(t, bin, "git", ": > "+shQuote(started)+"\nexec sleep 86400\n")
			guard := New(Options{
				Clock: clock,
				Env:   append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
				Visibility: func(context.Context, string) (Visibility, error) {
					return "", &rateLimitError{reset: reset}
				},
			})
			ctx := tc.caller(t, started)
			var err error
			answered(t, "the visibility read whose git never answers", func() { _, err = guard.Visibility(ctx, theRepo) })
			if err == nil {
				t.Fatal("the visibility read passed while git never answered")
			}
			for _, want := range []string{
				"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
				"GraphQL's rate limit is spent too, and git ls-remote could not answer: git ran ",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the failure %q does not say %q", err, want)
				}
			}
			tc.stop(t, err)
			if _, err := os.Stat(guard.visibilityCachePath(theRepo)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a visibility was kept for the refused read (%v), want none", err)
			}
		})
	}
}

// pktLine is payload as one pkt-line of git's protocol: its length, the four hex digits
// included, then the payload.
func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

// gitHubStandIn is a server of the test's own that stands in for GitHub's smart HTTP endpoint,
// answering by the repository's owner. "public" lists its refs to a request that carries no
// credential, and refuses one that carries any 401 with a challenge, as GitHub refuses a bad
// credential for a public repository. "private" answers every request 401 with a challenge, as
// GitHub answers an anonymous read of a private repository. "throttled" answers as GitHub did a
// public repository's anonymous clone under unusual traffic in GitHub's community discussion
// #206581: the advertisement's GET is answered, here with protocol version 2's, so git goes on
// to POST its ls-refs command, and the POST is answered 401 with GitHub's challenge. "forged"
// answers the GET with an ERR packet carrying git's own line for a server that asked for a
// credential. "forbidden" answers 403, and any other owner 404.
func gitHubStandIn(t *testing.T) *httptest.Server {
	t.Helper()
	head := strings.Repeat("1", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		switch {
		case owner == "public" && r.Header.Get("Authorization") == "" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(w, pktLine("# service=git-upload-pack\n")+"0000"+
				pktLine(head+" HEAD\x00agent=invented\n")+pktLine(head+" refs/heads/main\n")+"0000")
		case owner == "public", owner == "private":
			w.Header().Set("WWW-Authenticate", `Basic realm="invented"`)
			w.WriteHeader(http.StatusUnauthorized)
		case owner == "throttled" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(w, pktLine("version 2\n")+pktLine("agent=invented\n")+pktLine("ls-refs=unborn\n")+
				pktLine("fetch=shallow\n")+pktLine("object-format=sha1\n")+"0000")
		case owner == "throttled":
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			w.WriteHeader(http.StatusUnauthorized)
		case owner == "forged":
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(w, pktLine("# service=git-upload-pack\n")+"0000"+
				pktLine("ERR invented\nfatal: could not read Username for 'http://"+r.Host+"': terminal prompts disabled"))
		case owner == "forbidden":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// realGit is the absolute path of this machine's git, resolved once for the test, which is
// skipped where there is none.
func realGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	return git
}

// loopbackEnv is the environment of a guard whose real git reads a server of the test's own on
// 127.0.0.1, with home as the caller's home, path as its PATH entry and extra on top. The
// process's own git may use local files alone (TestMain) and the server is http, and no proxy the
// machine names may stand between git and the server.
func loopbackEnv(home, path string, extra ...string) []string {
	env := append(fixtureenv.Isolated(home), path, "GIT_ALLOW_PROTOCOL=http",
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	return append(env, extra...)
}

// The read with no credential answers public where a real git lists the refs, and nothing else.
// A server of the test's own stands in for GitHub's smart HTTP endpoint (gitHubStandIn), and the
// git on this machine reads it with the environment the guard gives it. A 401 for a private
// repository, a 401 for a public one under GitHub's throttle, and a server's ERR packet each make
// git print its line for a server that asked for a credential, the line this read once took for
// private; each is now no answer, as are a 403 and a 404.
func TestLsRemoteReadsWhatARealGitPrints(t *testing.T) {
	git := realGit(t)
	server := gitHubStandIn(t)
	guard := New(Options{Env: loopbackEnv(t.TempDir(), pathWith(filepath.Dir(git)))})
	for _, tc := range []struct {
		owner string
		// fails is what the error says, empty where the read answers public.
		fails string
	}{
		{owner: "public"},
		{owner: "private", fails: "could not read Username"},
		{owner: "throttled", fails: "could not read Username"},
		{owner: "forged", fails: "remote error: invented"},
		{owner: "forbidden", fails: "The requested URL returned error: 403"},
		{owner: "missing", fails: "not found"},
	} {
		t.Run(tc.owner, func(t *testing.T) {
			visibility, err := guard.lsRemote(bound(t), server.URL, tc.owner+"/invented-repo")
			if tc.fails == "" {
				if err != nil || visibility != Public {
					t.Fatalf("read %q, %v; want public", visibility, err)
				}
				return
			}
			if err == nil || visibility != "" {
				t.Fatalf("read %q, %v; want no answer", visibility, err)
			}
			if !strings.Contains(err.Error(), tc.fails) {
				t.Errorf("the error %q does not say %q", err, tc.fails)
			}
		})
	}
}

// No unauthenticated refusal reads as private, from the call's start: GitHub refuses the REST
// read for a spent core limit, GraphQL for a spent limit of its own, and ls-remote is made by the
// machine's real git against the stand-in server (gitHubStandIn), which a git wrapper first on
// PATH points https://github.com at with url.<base>.insteadOf. A public repository answers
// public; a throttled public repository's 401 and a server's ERR packet carrying git's old line
// refuse the call, naming the spent limit and its reset, and keep nothing.
func TestVisibilityNeverReadsAnUnauthenticatedRefusalAsPrivate(t *testing.T) {
	git := realGit(t)
	server := gitHubStandIn(t)
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(30 * time.Minute)
	for _, tc := range []struct {
		owner string
		// fails is what the refusal says of git, empty where the call answers public.
		fails string
	}{
		{owner: "public"},
		{owner: "throttled", fails: "could not read Username"},
		{owner: "forged", fails: "remote error: invented"},
	} {
		t.Run(tc.owner, func(t *testing.T) {
			slug := tc.owner + "/invented-repo"
			bin := t.TempDir()
			writeProgram(t, bin, "gh", graphQLRateLimited(filepath.Join(t.TempDir(), "graphql")))
			writeProgram(t, bin, "git", "exec "+shQuote(git)+" -c "+shQuote("url."+server.URL+"/.insteadOf="+githubOrigin+"/")+" \"$@\"\n")
			var sources []VisibilitySource
			guard := New(Options{
				Clock:        clock,
				OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
				Env:          loopbackEnv(t.TempDir(), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
				Visibility: func(context.Context, string) (Visibility, error) {
					return "", &rateLimitError{reset: reset}
				},
			})
			visibility, err := guard.Visibility(bound(t), slug)
			if tc.fails == "" {
				if err != nil || visibility != Public {
					t.Fatalf("read %q, %v; want public", visibility, err)
				}
				return
			}
			if err == nil || visibility != "" {
				t.Fatalf("read %q, %v; want the call refused", visibility, err)
			}
			for _, want := range []string{
				"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
				"GraphQL's rate limit is spent too, and git ls-remote could not answer",
				tc.fails,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the failure %q does not say %q", err, want)
				}
			}
			if len(sources) != 0 {
				t.Errorf("the guard reported the sources %q for a refused call, want none", sources)
			}
			if _, err := os.Stat(guard.visibilityCachePath(slug)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a visibility was kept for the refused call (%v), want none", err)
			}
		})
	}
}

// A setting a git wrapper adds does not make the read with no credential refuse a public
// repository, whether it is an extra header or http.proactiveAuth, and whether the wrapper sets it
// for no URL, for the host, or for the repository's URL with none, one or two slashes after it;
// nor does http.proactiveAuth in the caller's environment, which the read does not keep.
// `legion git --after` runs the host's next git on PATH, and a wrapper there can put a -c of its
// own in front of the guard's arguments. Here the machine's real git sits behind such a wrapper.
// The stand-in server refuses an Authorization header 401 for its public repository, as GitHub
// refuses a bad credential, and proactiveAuth makes git ask for a credential before it sends a
// request, so each setting that reached git would make it exit 128; the read answers public in
// every case. Each is a refusal the resets spare, never a private answer: with the generic header
// reset alone, the header for the host exits 128, and without the `//` header reset the header
// for the repository's URL and two slashes does; with the generic proactiveAuth reset alone, each
// proactiveAuth for a URL exits 128 (measured with git 2.47.3).
func TestLsRemoteClearsTheSettingsAGitWrapperAdds(t *testing.T) {
	git := realGit(t)
	server := gitHubStandIn(t)
	repo := server.URL + "/public/invented-repo.git"
	const header = "=Authorization: Basic aW52ZW50ZWQ6aW52ZW50ZWQ="
	for _, tc := range []struct {
		name string
		// setting is the -c the wrapper adds, empty for none, and env what the caller's
		// environment holds beyond the test's.
		setting string
		env     []string
	}{
		{name: "a header for no URL", setting: "http.extraHeader" + header},
		{name: "a header for the host", setting: "http." + server.URL + "/.extraHeader" + header},
		{name: "a header for the repository's URL", setting: "http." + repo + ".extraHeader" + header},
		{name: "a header for the repository's URL and a slash", setting: "http." + repo + "/.extraHeader" + header},
		{name: "a header for the repository's URL and two slashes", setting: "http." + repo + "//.extraHeader" + header},
		{name: "proactiveAuth basic for no URL", setting: "http.proactiveAuth=basic"},
		{name: "proactiveAuth basic for the host", setting: "http." + server.URL + "/.proactiveAuth=basic"},
		{name: "proactiveAuth auto for the host", setting: "http." + server.URL + "/.proactiveAuth=auto"},
		{name: "proactiveAuth basic for the repository's URL", setting: "http." + repo + ".proactiveAuth=basic"},
		{name: "proactiveAuth basic for the repository's URL and two slashes", setting: "http." + repo + "//.proactiveAuth=basic"},
		{name: "proactiveAuth basic in the caller's environment", env: []string{"GIT_CONFIG_PARAMETERS='http.proactiveauth'='basic'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, record := t.TempDir(), filepath.Join(t.TempDir(), "record")
			wrapper := "echo ran >> " + shQuote(record) + "\nexec " + shQuote(git)
			if tc.setting != "" {
				wrapper += " -c " + shQuote(tc.setting)
			}
			writeProgram(t, bin, "git", wrapper+" \"$@\"\n")
			guard := New(Options{Env: loopbackEnv(t.TempDir(), pathWith(bin), tc.env...)})
			visibility, err := guard.lsRemote(bound(t), server.URL, "public/invented-repo")
			if err != nil || visibility != Public {
				t.Fatalf("read %q, %v; want public: the setting reached the request", visibility, err)
			}
			if got := countLines(t, record); got != 1 {
				t.Errorf("the wrapper ran %d times, want once: the read must go through it", got)
			}
		})
	}
}
