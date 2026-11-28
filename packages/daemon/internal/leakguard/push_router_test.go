package leakguard

import (
	"io"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// A push that names a program or a proxy between git and the server keeps the base's bound
// (readsReachItsRepository): such a program sees whether it serves a read, git-upload-pack, or the
// push, git-receive-pack, and can send the two to different repositories, so the guard makes
// neither read of what the hook's URL holds, and the remote-tracking refs bound the push as they
// did before the reads (R10 of dispatch://INFRA-3524's spec, its tenth case). Each case's router
// routes by service: git-upload-pack reaches the hook's URL's destination, which holds a commit
// that adds the refused line, fetched by the destination's URL so that no remote-tracking ref
// holds it, and the new branch starts from it; git-receive-pack reaches a second bare repository
// that holds nothing. Reads made through the router would answer that the destination holds that
// commit and pass the push, which would land it in the second repository; the guard reads it and
// refuses the push, and the second repository is unchanged.
//
// Before the guard runs, each case shows that its router routes, or the case would prove nothing:
// git ls-remote of the hook's URL in the clone, under the case's setting, lists the destination's
// main, and a real git push of the branch by origin's name under it, with no hook, lands the
// branch in the second repository. That branch, and the tracking ref the push filed in the clone,
// are then deleted, so the guard meets both as they were.
//
// The routers: for ssh, a program that runs the upload-pack command git asks for as git wrote
// it, against the repository the URL's path names, and every receive-pack command against the
// second repository (sshRouter); for git://, a proxy program that reads git's request line and
// hands the connection, that line first, to a `git daemon --inetd` whose base path holds the
// destination, for upload-pack, or the second repository under the same name, for receive-pack
// (gitProxyRouter); for http, a forward proxy of the test's own that serves git's smart HTTP
// from the destination's directory for upload-pack and from the second's for receive-pack
// (byService, forwardProxy), with the invented host example.invalid in the hook's URL so that
// only the proxy reaches it; and for https, the same proxy, which answers CONNECT with a tunnel to
// a TLS server of its own that serves the same way, under GIT_SSL_NO_VERIFY=1.
//
// The hook's ssh:// and git:// URLs reach the destination without the push's program as well: an
// ssh:// one through the `ssh` first on the guard's PATH, which runs the remote command git asks
// for on this machine (sshRunsHere), and a git:// one served by git daemon on 127.0.0.1
// (servedByGitDaemon). So reads made without the push's program, as reads made outside the clone
// with no GIT_ name of the push's were, would answer for the destination too; the tester of
// dispatch://INFRA-3524 measured at 917b2071 that each of those five pushes then landed the
// refused commit in the second repository. A remote.origin.proxy applies to a push by origin's
// name and not to a read of a URL, so in its case the hook's URL is a server of the test's own on
// 127.0.0.1 that serves the destination alone, which a read of the URL reaches with no proxy, and
// the push by origin's name goes through the proxy to the second repository. No case's
// environment holds NO_PROXY or no_proxy, under which git and libcurl would pass a proxy by for the
// hosts they name, and an issue pod's environment names some (withoutNoProxy).
func TestAPushThatNamesAProgramOrProxyBetweenGitAndTheServerKeepsTheBasesBound(t *testing.T) {
	const class = leakguardtest.RulesHostClass
	refused := "the runner answers at " + leakguardtest.RulesHost + "\n"
	// proxied is the forward proxy of an http(s) case, routing by service between the
	// destination's directory and the second's.
	proxied := func(t *testing.T, r routes) string {
		t.Helper()
		return forwardProxy(t, byService(gitHTTP(t, r.root), gitHTTP(t, r.elsewhere)))
	}
	const httpURL, httpsURL = "http://example.invalid/destination.git", "https://example.invalid/destination.git"
	for _, tc := range []struct {
		name string
		// route makes the case's router for r and answers the URL git hands the hook, the clone's
		// configuration beyond its origin as keys and values, and what the push's environment holds
		// beyond fixtureenv.Isolated's.
		route func(t *testing.T, r routes) (url string, config, env []string)
	}{
		{
			name: "core.gitProxy",
			route: func(t *testing.T, r routes) (string, []string, []string) {
				return servedByGitDaemon(t, r.root) + "/destination.git", []string{"core.gitProxy", gitProxyRouter(t, r)},
					[]string{"GIT_ALLOW_PROTOCOL=file:git"}
			},
		},
		{
			name: "GIT_PROXY_COMMAND",
			route: func(t *testing.T, r routes) (string, []string, []string) {
				return servedByGitDaemon(t, r.root) + "/destination.git", nil,
					[]string{"GIT_ALLOW_PROTOCOL=file:git", "GIT_PROXY_COMMAND=" + gitProxyRouter(t, r)}
			},
		},
		{
			name: "core.sshCommand",
			route: func(t *testing.T, r routes) (string, []string, []string) {
				return "ssh://example.invalid" + r.destination, []string{"core.sshCommand", sshRouter(t, r.second)},
					[]string{"GIT_ALLOW_PROTOCOL=file:ssh", sshRunsHere(t)}
			},
		},
		{
			name: "GIT_SSH_COMMAND",
			route: func(t *testing.T, r routes) (string, []string, []string) {
				return "ssh://example.invalid" + r.destination, nil,
					[]string{"GIT_ALLOW_PROTOCOL=file:ssh", sshRunsHere(t), "GIT_SSH_COMMAND=" + sshRouter(t, r.second)}
			},
		},
		{
			name: "GIT_SSH",
			route: func(t *testing.T, r routes) (string, []string, []string) {
				return "ssh://example.invalid" + r.destination, nil,
					[]string{"GIT_ALLOW_PROTOCOL=file:ssh", sshRunsHere(t), "GIT_SSH=" + sshRouter(t, r.second)}
			},
		},
		{
			name: "http.proxy",
			route: func(t *testing.T, r routes) (string, []string, []string) {
				return httpURL, []string{"http.proxy", proxied(t, r)}, []string{"GIT_ALLOW_PROTOCOL=file:http"}
			},
		},
		{
			name: "http.<url>.proxy for the hook's URL",
			route: func(t *testing.T, r routes) (string, []string, []string) {
				return httpURL, []string{"http." + httpURL + ".proxy", proxied(t, r)}, []string{"GIT_ALLOW_PROTOCOL=file:http"}
			},
		},
		{
			name: "remote.origin.proxy",
			route: func(t *testing.T, r routes) (string, []string, []string) {
				direct := httptest.NewServer(gitHTTP(t, r.root))
				t.Cleanup(direct.Close)
				return direct.URL + "/destination.git", []string{"remote.origin.proxy", proxied(t, r)},
					[]string{"GIT_ALLOW_PROTOCOL=file:http"}
			},
		},
		{
			name: "http_proxy",
			route: func(t *testing.T, r routes) (string, []string, []string) {
				return httpURL, nil, []string{"GIT_ALLOW_PROTOCOL=file:http", "http_proxy=" + proxied(t, r)}
			},
		},
		{
			name: "https_proxy",
			route: func(t *testing.T, r routes) (string, []string, []string) {
				return httpsURL, nil, []string{"GIT_ALLOW_PROTOCOL=file:https", "GIT_SSL_NO_VERIFY=1", "https_proxy=" + proxied(t, r)}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, elsewhere := t.TempDir(), t.TempDir()
			r := routes{
				root:        root,
				elsewhere:   elsewhere,
				destination: filepath.Join(root, "destination.git"),
				second:      filepath.Join(elsewhere, "destination.git"),
			}
			fixture := fixturegit.Isolated(t, t.TempDir())
			seed, repo := filepath.Join(root, "seed"), filepath.Join(root, "repo")
			fixture.Run(root, fixturegit.InitBare(r.destination, "main")...)
			fixture.Run(elsewhere, fixturegit.InitBare(r.second, "main")...)
			// git's smart HTTP takes a push only into a repository that says it takes one.
			fixture.Run(r.second, "git", "config", "http.receivepack", "true")
			fixture.Run(root, fixturegit.InitWorkTree(seed, "main")...)
			commitFiles(t, fixture, seed, map[string]string{"README.md": "one\n"}, "the destination's first commit")
			fixture.Run(seed, "git", "push", "--quiet", r.destination, "main")
			fixture.Run(root, "git", "clone", "--quiet", r.destination, repo)
			commitFiles(t, fixture, seed, map[string]string{"held.md": refused}, "a commit of the destination's that adds the refused line")
			fixture.Run(seed, "git", "push", "--quiet", r.destination, "main")
			fixture.Run(repo, "git", "fetch", "--quiet", r.destination, "main")
			start := fixture.Run(repo, "git", "rev-parse", "FETCH_HEAD")
			url, config, extra := tc.route(t, r)
			fixture.Run(repo, "git", "remote", "set-url", "origin", url)
			for i := 0; i+1 < len(config); i += 2 {
				fixture.Run(repo, "git", "config", config[i], config[i+1])
			}
			fixture.Run(repo, "git", "checkout", "--quiet", "-b", "work", start)
			own := commitFiles(t, fixture, repo, map[string]string{"work.md": "a line of the branch's own\n"}, "the branch's own commit")
			env := append(withoutNoProxy(fixtureenv.Isolated(t.TempDir())), extra...)
			routed := fixturegit.Runner{TB: t, Env: env}
			if listed, want := routed.Out(repo, "git", "ls-remote", "--heads", url), start+"\trefs/heads/main\n"; listed != want {
				t.Fatalf("git ls-remote of %s through %s lists %q, want the destination's main %q: the router does not route git-upload-pack there, and the case would prove nothing", url, tc.name, listed, want)
			}
			routed.Run(repo, "git", "-c", "core.hooksPath=/dev/null", "push", "--quiet", "origin", "work")
			if landed, _ := fixture.Try(r.second, "git", "rev-parse", "--verify", "--quiet", "refs/heads/work"); strings.TrimSpace(landed) != own {
				t.Fatalf("the push through %s left the second repository's work at %q, want %s: the router does not route git-receive-pack there, and the case would prove nothing", tc.name, landed, own)
			}
			fixture.Run(r.second, "git", "update-ref", "-d", "refs/heads/work")
			fixture.Run(repo, "git", "update-ref", "-d", "refs/remotes/origin/work")
			before := fixture.Raw(r.second, "git", "for-each-ref")
			guard := New(Options{Check: leakguardtest.Write(t, t.TempDir(), passes), Visibility: answers(Public, nil), Env: withRules(t, env)})
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     url,
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: own, RemoteRef: "refs/heads/work", RemoteSHA: noSHA}},
			})
			if err == nil {
				t.Fatalf("the push through %s passed: the guard read what %s holds through the router, which sends the push elsewhere, and took the refused commit as held", tc.name, url)
			}
			reports(t, refusal(t, err).Lines, "held.md: "+class)
			if after := fixture.Raw(r.second, "git", "for-each-ref"); after != before {
				t.Errorf("the second repository's refs were %q before the guard's read of the push and are %q after it", before, after)
			}
		})
	}
}

// routes are the two repositories of a case of
// TestAPushThatNamesAProgramOrProxyBetweenGitAndTheServerKeepsTheBasesBound, each named
// destination.git in a directory of its own: the destination at root, which the hook's URL names
// and git-upload-pack reaches, and the second at elsewhere, which git-receive-pack reaches.
type routes struct {
	root, elsewhere     string
	destination, second string
}

// sshRouter is an ssh program of the push's own, written to a directory of the test's own, that
// routes by service: the git-upload-pack command git asks for runs here as git wrote it, against
// the repository the URL's path names, and every git-receive-pack command runs against second.
// It answers `-G`, by which git asks a program it does not know whether it is OpenSSH, as OpenSSH
// does, so that git hands it the protocol version it speaks in GIT_PROTOCOL, as it hands ssh.
func sshRouter(t *testing.T, second string) string {
	t.Helper()
	dir := t.TempDir()
	writeProgram(t, dir, "ssh-router", `[ "$1" = -G ] && exit 0
for last; do :; done
case "$last" in
"git-upload-pack "*) exec sh -c "$last" ;;
"git-receive-pack "*) exec `+shQuote(fixturegit.CompiledGit(t))+` receive-pack `+shQuote(second)+` ;;
esac
exit 1
`)
	return filepath.Join(dir, "ssh-router")
}

// sshRunsHere is a PATH entry whose `ssh`, written to a directory of the test's own, runs its
// last argument, the remote command git asks for, on this machine, so an ssh:// URL whose path is
// a repository here reaches that repository with no ssh program of the push's own.
func sshRunsHere(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	writeProgram(t, bin, "ssh", "for last; do :; done\nexec sh -c \"$last\"\n")
	return pathWith(bin)
}

// servedByGitDaemon serves the repositories under base over git:// on 127.0.0.1, each connection
// by a `git daemon --inetd` of its own, until the test ends, and answers git://<address>.
func servedByGitDaemon(t *testing.T, base string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	git := fixturegit.CompiledGit(t)
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		var served sync.WaitGroup
		defer served.Wait()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			file, err := conn.(*net.TCPConn).File()
			_ = conn.Close()
			if err != nil {
				continue
			}
			served.Add(1)
			go func() {
				defer served.Done()
				defer file.Close()
				cmd := exec.Command(git, "daemon", "--inetd", "--export-all", "--log-destination=none", "--base-path="+base)
				cmd.Stdin, cmd.Stdout, cmd.Env = file, file, os.Environ()
				_ = cmd.Run()
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-accepted
	})
	return "git://" + listener.Addr().String()
}

// gitProxyRouter is a git:// proxy program of the push's own, written to a directory of the
// test's own, that routes by service: it reads git's request line, a pkt-line whose first four
// bytes are its length in hex and whose next sixteen name the service, keeps it in a file of
// that directory, and hands the connection, that line first, to a `git daemon --inetd` whose
// base path is r.root, which holds the destination, for git-upload-pack, and r.elsewhere, which
// holds the second repository under the same name, for git-receive-pack. git writes nothing more
// until the daemon answers that line, so the read takes no byte of what follows it.
func gitProxyRouter(t *testing.T, r routes) string {
	t.Helper()
	dir := t.TempDir()
	writeProgram(t, dir, "git-proxy-router", `request=$(mktemp `+shQuote(filepath.Join(dir, "request.XXXXXX"))+`) || exit 1
head -c 4 > "$request"
head -c $((0x$(cat "$request") - 4)) >> "$request"
case "$(dd if="$request" bs=1 skip=4 count=16 2>/dev/null)" in
"git-upload-pack "*) base=`+shQuote(r.root)+` ;;
git-receive-pack) base=`+shQuote(r.elsewhere)+` ;;
*) exit 1 ;;
esac
{ cat "$request"; rm -f "$request"; exec cat; } |
	exec `+shQuote(fixturegit.CompiledGit(t))+` daemon --inetd --export-all --enable=receive-pack --log-destination=none --base-path="$base"
`)
	return filepath.Join(dir, "git-proxy-router")
}

// gitHTTP is git's smart HTTP for the repositories under root: the git-http-backend of the
// compiled git's exec path, run as a CGI program under a home of the test's own and no system
// configuration, with every repository exported and each repository's own http.receivepack
// deciding whether it takes a push.
func gitHTTP(t *testing.T, root string) http.Handler {
	t.Helper()
	execPath := fixturegit.Isolated(t, t.TempDir()).Run(t.TempDir(), fixturegit.CompiledGit(t), "--exec-path")
	return &cgi.Handler{
		Path: filepath.Join(execPath, "git-http-backend"),
		Env: []string{
			"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1",
			"HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		},
	}
}

// byService serves each request of git's smart HTTP for git-upload-pack with upload and each for
// git-receive-pack with receive, the service read as git names it, from the request's service=
// (`info/refs?service=<service>`) and else from the last element of its path
// (`<repository>/<service>`). It answers any other request 404.
func byService(upload, receive http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service := r.URL.Query().Get("service")
		if service == "" {
			service = path.Base(r.URL.Path)
		}
		switch service {
		case "git-upload-pack":
			upload.ServeHTTP(w, r)
		case "git-receive-pack":
			receive.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// forwardProxy is an HTTP proxy of the test's own on 127.0.0.1, until the test ends, and answers
// its URL. It serves every request it is sent with route itself rather than forwarding it, and
// answers CONNECT with a tunnel to a TLS server of its own, which serves with route too, so that
// a request for any host, an https:// one included, reaches route.
func forwardProxy(t *testing.T, route http.Handler) string {
	t.Helper()
	tlsServer := httptest.NewTLSServer(route)
	t.Cleanup(tlsServer.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			route.ServeHTTP(w, r)
			return
		}
		upstream, err := net.Dial("tcp", tlsServer.Listener.Addr().String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
			return
		}
		go func() {
			_, _ = io.Copy(upstream, buffered.Reader)
			_ = upstream.(*net.TCPConn).CloseWrite()
		}()
		_, _ = io.Copy(conn, upstream)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL
}

// withoutNoProxy is env with no NO_PROXY and no no_proxy, under which git and libcurl would pass
// a proxy by for the hosts they name.
func withoutNoProxy(env []string) []string {
	return slices.DeleteFunc(env, func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return name == "NO_PROXY" || name == "no_proxy"
	})
}
