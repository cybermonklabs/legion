package leakguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// A push to a URL git reaches over ssh always goes through a program between git and the
// server: the `ssh` git runs, which, where the push names no ssh command of its own
// (core.sshCommand, GIT_SSH_COMMAND, GIT_SSH), is the first `ssh` on PATH. That program sees
// whether it serves a read, git-upload-pack, or the push, git-receive-pack, as the ssh command the
// push names does, and can send the two to different repositories, so a read of the hook's URL
// through it need not reach the repository the push writes to. R10 of dispatch://INFRA-3524's spec
// makes the two reads only where they reach that repository; for every other push the base's
// bound stands.
//
// Each case's `ssh` first on PATH is sshRouter's program, which routes by service, and the push
// names no setting at all. Its destination holds a commit that adds the refused line, fetched by
// the destination's URL so that no remote-tracking ref holds it, and the new branch starts from
// it; the second repository holds nothing. As in
// TestAPushThatNamesAProgramOrProxyBetweenGitAndTheServerKeepsTheBasesBound, each case first shows
// that its router routes: git ls-remote of the hook's URL lists the destination's main, and a real
// push by origin's name, with no hook, lands the branch in the second repository. Reads made
// through the router answer that the destination holds the refused commit and pass the push, which
// would land it in the second repository; the base reads that commit and refuses the push, and so
// must the guard. Measured by INFRA-3524's tester on 2026-11-28 at 33a24deb, through the head's git
// wrapper and hook: such a push to ssh://example.invalid<path> landed the refused commit in the
// second repository, and main's hook refused it.
func TestAPushOverSSHThroughTheSSHOnPathKeepsTheBasesBound(t *testing.T) {
	const class = leakguardtest.RulesHostClass
	refused := "the runner answers at " + leakguardtest.RulesHost + "\n"
	for _, tc := range []struct {
		name string
		// url is the URL git hands the hook for the destination at path.
		url func(path string) string
	}{
		{name: "an ssh:// URL", url: func(path string) string { return "ssh://example.invalid" + path }},
		{name: "the scp-like host:path", url: func(path string) string { return "example.invalid:" + path }},
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
			fixture.Run(root, fixturegit.InitWorkTree(seed, "main")...)
			commitFiles(t, fixture, seed, map[string]string{"README.md": "one\n"}, "the destination's first commit")
			fixture.Run(seed, "git", "push", "--quiet", r.destination, "main")
			fixture.Run(root, "git", "clone", "--quiet", r.destination, repo)
			commitFiles(t, fixture, seed, map[string]string{"held.md": refused}, "a commit of the destination's that adds the refused line")
			fixture.Run(seed, "git", "push", "--quiet", r.destination, "main")
			fixture.Run(repo, "git", "fetch", "--quiet", r.destination, "main")
			start := fixture.Run(repo, "git", "rev-parse", "FETCH_HEAD")
			url := tc.url(r.destination)
			fixture.Run(repo, "git", "remote", "set-url", "origin", url)
			fixture.Run(repo, "git", "checkout", "--quiet", "-b", "work", start)
			own := commitFiles(t, fixture, repo, map[string]string{"work.md": "a line of the branch's own\n"}, "the branch's own commit")
			// The `ssh` first on PATH routes by service; the push names no ssh command.
			bin := t.TempDir()
			if err := os.Symlink(sshRouter(t, r.second), filepath.Join(bin, "ssh")); err != nil {
				t.Fatal(err)
			}
			env := append(withoutNoProxy(fixtureenv.Isolated(t.TempDir())), "GIT_ALLOW_PROTOCOL=file:ssh", pathWith(bin))
			routed := fixturegit.Runner{TB: t, Env: env}
			if listed, want := routed.Out(repo, "git", "ls-remote", "--heads", url), start+"\trefs/heads/main\n"; listed != want {
				t.Fatalf("git ls-remote of %s through the ssh on PATH lists %q, want the destination's main %q: the router does not route git-upload-pack there, and the case would prove nothing", url, listed, want)
			}
			routed.Run(repo, "git", "-c", "core.hooksPath=/dev/null", "push", "--quiet", "origin", "work")
			if landed, _ := fixture.Try(r.second, "git", "rev-parse", "--verify", "--quiet", "refs/heads/work"); strings.TrimSpace(landed) != own {
				t.Fatalf("the push through the ssh on PATH left the second repository's work at %q, want %s: the router does not route git-receive-pack there, and the case would prove nothing", landed, own)
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
				t.Fatalf("the push to %s passed: the guard read what it holds through the ssh on PATH, which sends the push elsewhere, and took the refused commit as held", url)
			}
			reports(t, refusal(t, err).Lines, "held.md: "+class)
			if after := fixture.Raw(r.second, "git", "for-each-ref"); after != before {
				t.Errorf("the second repository's refs were %q before the guard's read of the push and are %q after it", before, after)
			}
		})
	}
}
