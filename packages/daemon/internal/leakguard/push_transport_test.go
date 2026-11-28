package leakguard

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// A push whose transport git connects through a program the push names, an ssh command
// (core.sshCommand, GIT_SSH_COMMAND, GIT_SSH) or a proxy for git:// (core.gitProxy,
// GIT_PROXY_COMMAND), writes where that program connects, which need not be the repository the
// hook's URL names. The guard's two reads of what the destination holds (destinationHolds) are
// made outside the clone's configuration and drop every GIT_* name but three (lsRemoteInherits),
// so they connect to the URL without that program, and answer for a repository the push need not
// write to. R10 of dispatch://INFRA-3524's spec makes the reads only for a push that writes where
// the hook's URL names; for every other push the base's bound stands, and a read of the hook's
// URL never stands for a destination the push does not write to.
//
// Each case's destination holds a commit that adds the refused line, fetched by the destination's
// URL so that no remote-tracking ref holds it, and the new branch starts from it. The URL the hook
// is handed reaches that destination without the push's program: an ssh:// URL through the `ssh`
// first on the guard's PATH, which runs the remote command on this machine, and a git:// URL
// served by git daemon on 127.0.0.1. The push's own program would serve a second bare repository
// that holds nothing. So the reads, were they made, would answer that the destination holds the
// refused commit, and pass the push; the base reads that commit and refuses it, and so must the
// guard. Measured by INFRA-3524's tester on 2026-11-28 at 917b2071, through the head's git
// wrapper and hook with a rule file named and no checker: each of the five pushes landed the
// refused commit in the second repository, and main's hook refused each.
func TestAPushWhoseTransportProgramCanConnectElsewhereKeepsTheBasesBound(t *testing.T) {
	const class = leakguardtest.RulesHostClass
	refused := "the runner answers at " + leakguardtest.RulesHost + "\n"
	// sshRunsHere is an `ssh` that runs its last argument, the remote command git asks for, on
	// this machine, so an ssh:// URL whose path is a repository here reaches that repository.
	sshRunsHere := func(t *testing.T) string {
		t.Helper()
		bin := t.TempDir()
		writeProgram(t, bin, "ssh", "for last; do :; done\nexec sh -c \"$last\"\n")
		return pathWith(bin)
	}
	// sshElsewhere is an ssh command of the push's own that serves every push from a bare
	// repository of its own, which holds nothing.
	sshElsewhere := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		second := filepath.Join(dir, "elsewhere.git")
		fixturegit.Isolated(t, t.TempDir()).Run(dir, fixturegit.InitBare(second, "main")...)
		writeProgram(t, dir, "ssh-elsewhere", "exec git-receive-pack "+shQuote(second)+"\n")
		return filepath.Join(dir, "ssh-elsewhere")
	}
	// proxyElsewhere is a git:// proxy of the push's own that serves every request from a
	// directory of its own, whose destination.git holds nothing.
	proxyElsewhere := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		fixturegit.Isolated(t, t.TempDir()).Run(dir, fixturegit.InitBare(filepath.Join(dir, "destination.git"), "main")...)
		writeProgram(t, dir, "proxy-elsewhere", "exec "+shQuote(fixturegit.CompiledGit(t))+
			" daemon --inetd --export-all --enable=receive-pack --log-destination=none --base-path="+shQuote(dir)+"\n")
		return filepath.Join(dir, "proxy-elsewhere")
	}
	for _, tc := range []struct {
		name string
		// url is the URL git hands the hook, which reaches the case's destination under root.
		url func(t *testing.T, root string) string
		// config is the clone's own configuration beyond its origin, as key and value.
		config func(t *testing.T) []string
		// env is what the guard's environment holds beyond guardWith's.
		env func(t *testing.T) []string
	}{
		{
			name: "core.sshCommand",
			url: func(t *testing.T, root string) string {
				return "ssh://example.invalid" + filepath.Join(root, "destination.git")
			},
			config: func(t *testing.T) []string { return []string{"core.sshCommand", sshElsewhere(t)} },
			env:    func(t *testing.T) []string { return []string{sshRunsHere(t), "GIT_ALLOW_PROTOCOL=file:ssh"} },
		},
		{
			name: "GIT_SSH_COMMAND",
			url: func(t *testing.T, root string) string {
				return "ssh://example.invalid" + filepath.Join(root, "destination.git")
			},
			env: func(t *testing.T) []string {
				return []string{sshRunsHere(t), "GIT_ALLOW_PROTOCOL=file:ssh", "GIT_SSH_COMMAND=" + sshElsewhere(t)}
			},
		},
		{
			name: "GIT_SSH",
			url: func(t *testing.T, root string) string {
				return "ssh://example.invalid" + filepath.Join(root, "destination.git")
			},
			env: func(t *testing.T) []string {
				return []string{sshRunsHere(t), "GIT_ALLOW_PROTOCOL=file:ssh", "GIT_SSH=" + sshElsewhere(t)}
			},
		},
		{
			name:   "core.gitProxy",
			url:    func(t *testing.T, root string) string { return servedByGitDaemon(t, root) + "/destination.git" },
			config: func(t *testing.T) []string { return []string{"core.gitProxy", proxyElsewhere(t)} },
			env:    func(t *testing.T) []string { return []string{"GIT_ALLOW_PROTOCOL=file:git"} },
		},
		{
			name: "GIT_PROXY_COMMAND",
			url:  func(t *testing.T, root string) string { return servedByGitDaemon(t, root) + "/destination.git" },
			env: func(t *testing.T) []string {
				return []string{"GIT_ALLOW_PROTOCOL=file:git", "GIT_PROXY_COMMAND=" + proxyElsewhere(t)}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fixture := fixturegit.Isolated(t, t.TempDir())
			destination, seed, repo := filepath.Join(root, "destination.git"), filepath.Join(root, "seed"), filepath.Join(root, "repo")
			fixture.Run(root, fixturegit.InitBare(destination, "main")...)
			fixture.Run(root, fixturegit.InitWorkTree(seed, "main")...)
			commitFiles(t, fixture, seed, map[string]string{"README.md": "one\n"}, "the destination's first commit")
			fixture.Run(seed, "git", "push", "--quiet", destination, "main")
			fixture.Run(root, "git", "clone", "--quiet", destination, repo)
			commitFiles(t, fixture, seed, map[string]string{"held.md": refused}, "a commit of the destination's that adds the refused line")
			fixture.Run(seed, "git", "push", "--quiet", destination, "main")
			fixture.Run(repo, "git", "fetch", "--quiet", destination, "main")
			start := fixture.Run(repo, "git", "rev-parse", "FETCH_HEAD")
			url := tc.url(t, root)
			fixture.Run(repo, "git", "remote", "set-url", "origin", url)
			if tc.config != nil {
				kv := tc.config(t)
				fixture.Run(repo, "git", "config", kv[0], kv[1])
			}
			fixture.Run(repo, "git", "checkout", "--quiet", "-b", "work", start)
			own := commitFiles(t, fixture, repo, map[string]string{"work.md": "a line of the branch's own\n"}, "the branch's own commit")
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, tc.env(t)...)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     url,
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: own, RemoteRef: "refs/heads/work", RemoteSHA: noSHA}},
			})
			if err == nil {
				t.Fatalf("the push through %s passed: the guard read what %s holds, which the push need not write to, and took the refused commit as held", tc.name, url)
			}
			reports(t, refusal(t, err).Lines, "held.md: "+class)
		})
	}
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
