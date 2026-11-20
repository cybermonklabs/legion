// The Stage 4a harness's repository cache checks: the cache the run's first issue pod made at
// root-ready serves its repository only with its token, a second pod fills its feed from it alone,
// asking GitHub only for the delta, and an issue whose volume is lost is provisioned again through
// the cache onto a commit of its pushed branch that neither the default branch nor the cache holds.
// The rig is live_test.go.

package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// cacheStreamLine is the fetch container's last line when it filled the feed from the repository
// cache: the tar's bytes, the cache's address, and the bytes and refs GitHub's delta brought.
var cacheStreamLine = regexp.MustCompile(`workspace-init fetch: ([0-9,]+) bytes from the repository cache (http://\S+) in \S+, its history verified in \S+; ([0-9,]+) bytes from GitHub for the delta \(([0-9]+) refs?\)`)

// repository-cache: the cache the root's launch made (root-ready) is Ready as the runtime's stores
// show it, at its Service, which the cluster holds as the controller made it — controlled by the
// cache's Sandbox, selecting the hash of its name, and headless with no external name, the shape
// the launch checked before telling
// the root's pod the cache's address. Asked inside the cache's own pod, its /healthz answers with
// no token, ready with a fetch no older than its refresh and two minutes, at the head GitHub named
// for the repository's default branch at that fetch (headAtFetch), and its /tar refuses a request
// without its token, 401 naming nothing, and answers its own token, which reaches curl on its
// standard input (`-H @-`) from the file the pod mounts, never in a process's argv.
func (r *liveRig) checkRepositoryCache() error {
	name := CacheSandboxName(r.env.project, r.env.repo)
	status, err := r.awaitCacheReady()
	if err != nil {
		return err
	}
	if want := r.rt.cacheURL(name); status.Address != want {
		return fmt.Errorf("the repository cache %s is Ready at %q, want its Service %s", name, status.Address, want)
	}
	service, err := r.kubectl("get", "service", name, "-o", `jsonpath={.metadata.ownerReferences[?(@.controller==true)].kind}/{.metadata.ownerReferences[?(@.controller==true)].name} {.spec.selector} clusterIP={.spec.clusterIP} externalName={.spec.externalName}`)
	if err != nil {
		return err
	}
	if want := "Sandbox/" + name + ` {"` + sandboxNameHashLabel + `":"` + sandboxNameHash(name) + `"} clusterIP=None externalName=`; strings.TrimSpace(service) != want {
		return fmt.Errorf("the cluster holds the cache's Service as %q, want %q", strings.TrimSpace(service), want)
	}
	health, body, err := r.cacheHealth(name)
	if err != nil {
		return err
	}
	curl := fmt.Sprintf(`curl -s -w ' %%{http_code}' localhost:%d`, repocache.Port)
	head, err := r.githubHead(r.defaultBranch)
	if err != nil {
		return err
	}
	if !health.Ready || health.Repo != r.env.repo.String() || health.Bytes <= 0 || health.LastFetchAt.IsZero() {
		return fmt.Errorf("the cache's /healthz answered %+v, want ready for %s", health, r.env.repo)
	}
	if refresh := liveRepositoryCache().Refresh; time.Since(health.LastFetchAt) > refresh+2*time.Minute {
		return fmt.Errorf("the cache's last fetch was at %s, more than its %s refresh and two minutes ago", health.LastFetchAt.Format(time.RFC3339), refresh)
	}
	if health.Head != head {
		return fmt.Errorf("the cache serves %s from its fetch at %s, but the default branch %s is at %s, which nothing of the run moves", health.Head, health.LastFetchAt.Format(time.RFC3339), r.defaultBranch, head)
	}
	tar := strings.Replace(curl, "curl -s", "curl -s -o /dev/null", 1) + "/tar"
	refused, err := r.kubectl("exec", name, "-c", cacheContainer, "--", "sh", "-c", strings.Replace(curl, "curl -s", "curl -s -D -", 1)+"/tar")
	if err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSpace(refused), " 401") || strings.Contains(refused, r.env.repo.String()) {
		return fmt.Errorf("the cache's /tar without its token answered %q, want 401 naming nothing", strings.TrimSpace(refused))
	}
	withToken := `{ printf 'Authorization: Bearer '; tr -d '\r\n' <"$` + repocache.CacheTokenFileEnv + `"; echo; } | ` + strings.Replace(tar, "curl -s", "curl -s -H @-", 1)
	served, err := r.kubectl("exec", name, "-c", cacheContainer, "--", "sh", "-c", withToken)
	if err != nil {
		return err
	}
	if strings.TrimSpace(served) != "200" {
		return fmt.Errorf("the cache's /tar with its token answered %q, want 200", strings.TrimSpace(served))
	}
	note("runtime", "RepositoryCache(%s): %s at %s; its Service %s", r.env.repo, status.State, status.Address, strings.TrimSpace(service))
	note("operator", "exec %s -c %s -- curl localhost:%d: /healthz with no token %s; /tar 401 without its token, %s with it on stdin; GitHub's %s is %s", name, cacheContainer, repocache.Port, body, strings.TrimSpace(served), r.defaultBranch, head)
	return nil
}

// cacheHealth is the cache name's /healthz, asked inside its own pod with no token, decoded, with
// the body as it answered: an error unless it answered 200 and its JSON.
func (r *liveRig) cacheHealth(name string) (repocache.Health, string, error) {
	out, err := r.kubectl("exec", name, "-c", cacheContainer, "--", "sh", "-c", fmt.Sprintf(`curl -s -w ' %%{http_code}' localhost:%d/healthz`, repocache.Port))
	if err != nil {
		return repocache.Health{}, "", err
	}
	body, code, _ := strings.Cut(strings.TrimSpace(out), " ")
	var health repocache.Health
	if err := json.Unmarshal([]byte(body), &health); err != nil || code != "200" {
		return repocache.Health{}, "", fmt.Errorf("the cache %s's /healthz with no token answered %q, not 200 and its JSON: %v", name, strings.TrimSpace(out), err)
	}
	return health, body, nil
}

// cache-second-pod: another issue pod of the run's repository (cache-b), launched while the cache
// is Ready, fills its feed from the cache's tar and fetches from GitHub only the delta, under 1 MiB.
// Its clone owns its objects rather than borrowing them from the feed, and git verifies that
// connectivity after the feed is mounted read-only.
func (r *liveRig) checkCacheSecondPod() error {
	if _, err := r.awaitCacheReady(); err != nil {
		return err
	}
	c := r.claim("cache-b")
	if err := r.ensureRunning(c); err != nil {
		return err
	}
	if err := r.assertCacheFed(c); err != nil {
		return err
	}
	clone := TreeRoot + "/repos/github.com/" + r.env.repo.Owner() + "/" + r.env.repo.Name()
	if _, err := r.exec(c, "test", "!", "-e", clone+"/.git/objects/info/alternates"); err != nil {
		return fmt.Errorf("cache-b's clone still borrows objects from the feed: %w", err)
	}
	fsck, err := r.exec(c, "git", "--git-dir="+clone+"/.git", "fsck", "--connectivity-only", "--no-progress")
	if err != nil {
		return fmt.Errorf("git fsck of cache-b's clone: %w", err)
	}
	note("operator", "exec %s -- test ! -e %s/.git/objects/info/alternates; git fsck --connectivity-only: ok %s", SandboxName(c.token), clone, oneLine(fsck))
	return nil
}

// cache-lost-volume: with the cache warm, the harness pushes cache-b's branch, legion/<issue> for
// its run's own issue (cacheRunIssue), onto the repository as a commit of its own — a file only
// that commit adds, on top of the default branch's head — and checks inside the cache's pod that
// its clone lacks that commit.
// cache-b's Sandbox is then deleted while no runtime runs, which takes its pod and, with the pod,
// the disk its workspace was on — what every stop of an issue now leaves. Relaunched,
// the new pod fills its new feed from the cache again, and, while the cache still lacks the
// commit, GitHub's delta brings it (more than no bytes); the workspace is provisioned on the
// branch the issue pushed, which is where its work survived: its @- is that commit, and the file
// that commit added is in it.
func (r *liveRig) checkCacheLostVolume() error {
	status, err := r.awaitCacheReady()
	if err != nil {
		return err
	}
	c := r.claim("cache-b")
	if err := r.ensureRunning(c); err != nil {
		return err
	}
	dir, err := workspace.Location(TreeRoot, r.env.repo, c.issue)
	if err != nil {
		return err
	}
	base, err := r.githubHead(r.defaultBranch)
	if err != nil {
		return err
	}
	health, _, err := r.cacheHealth(status.Sandbox)
	if err != nil {
		return err
	}
	cached := health.Head
	if cached == "" {
		return fmt.Errorf("the cache %s's /healthz names no head: %+v", status.Sandbox, health)
	}
	marker, content := ".legion-e2e/"+c.issue+".txt", fmt.Sprintf("the commit only %s holds, pushed at %s\n", dir.Bookmark, time.Now().UTC().Format(time.RFC3339Nano))
	branchHead, err := r.pushRunCommit(dir.Bookmark, base, marker, content)
	if err != nil {
		return err
	}
	if branchHead == base || branchHead == cached {
		return fmt.Errorf("the pushed %s is %s, which the default branch (%s) or the cache (%s) already holds", dir.Bookmark, branchHead, base, cached)
	}
	if held, err := r.cacheHolds(status.Sandbox, branchHead); err != nil || held {
		return fmt.Errorf("the cache %s holds the just-pushed %s already (%v), so the recovery would prove nothing of GitHub's delta", status.Sandbox, branchHead, err)
	}
	note("operator", "pushed %s on %s at %s, adding %s on the default branch's %s (the cache serves %s and lacks %s)", dir.Bookmark, r.env.repo, branchHead, marker, base, cached, branchHead)

	r.stopRuntime()
	name := SandboxName(c.token)
	if _, err := r.kubectl("delete", "sandbox", name, "--wait=true", "--timeout=5m"); err != nil {
		return err
	}
	if err := r.awaitDiskGone(c); err != nil {
		return err
	}
	c.loc, c.last, c.state = nil, nil, stateNone
	note("operator", "deleted Sandbox %s while no runtime ran; its pod's own disk %s went with it", name, issueDiskName(c.token))

	if err := r.startRuntime(); err != nil {
		return err
	}
	if err := r.rt.ReconcileOrphans(r.ctx, r.known(c), time.Hour); err != nil {
		return err
	}
	if _, err := r.awaitCacheReady(); err != nil {
		return err
	}
	since := time.Now()
	c.bootToken, c.gen = r.reg.mint(c.token, true)
	// A plain Spawn: nothing tells the launch that a disk was lost any more, since every launch of
	// an issue starts on a disk of its own. workspace-init provisions on the issue's branch
	// because origin holds one (internal/workspace's Provision), not because this names it.
	loc, err := r.rt.Spawn(r.ctx, r.spec(c, ""))
	if err != nil {
		return err
	}
	c.loc, c.last, c.state = &loc, &loc, stateRunning
	if _, err := r.awaitRunning(c, since); err != nil {
		return err
	}
	if err := r.assertCacheFed(c); err != nil {
		return err
	}
	if held, err := r.cacheHolds(status.Sandbox, branchHead); err != nil {
		return err
	} else if !held {
		fetched, err := r.fetchLog(name)
		if err != nil {
			return err
		}
		if match := cacheStreamLine.FindStringSubmatch(fetched); match == nil || printedCount(match[3]) <= 0 {
			return fmt.Errorf("the cache still lacks %s, yet the recovered pod's fetch brought nothing from GitHub: %s", branchHead, strings.TrimSpace(fetched))
		}
	}
	log, err := r.initLog(name)
	if err != nil {
		return err
	}
	if want := "workspace-init: " + dir.Dir + " on " + dir.Bookmark + " from " + r.defaultBranch + "\n"; !strings.Contains(log, want) {
		return fmt.Errorf("the recovered pod's init log lacks %q: %s", strings.TrimSpace(want), strings.TrimSpace(log))
	}
	parent, err := r.exec(c, "jj", "-R", dir.Dir, "log", "-r", "@-", "--no-graph", "-T", "commit_id", "--ignore-working-copy")
	if err != nil {
		return err
	}
	if parent != branchHead {
		return fmt.Errorf("the recovered workspace's @- is %q, want %s's commit %s (the default branch is %s)", parent, dir.Bookmark, branchHead, base)
	}
	held, err := r.exec(c, "cat", dir.Dir+"/"+marker)
	if err != nil || held != strings.TrimSpace(content) {
		return fmt.Errorf("the recovered workspace's %s holds %q (%v), want %q", marker, held, err, strings.TrimSpace(content))
	}
	note("runtime", "Spawn(%s) into a new Sandbox, a new pod and a disk of that pod's own; its workspace stands on %s", c.token, dir.Bookmark)
	note("operator", "init log: %s; workspace @- %s = %s, not the default branch's %s; %s holds the pushed line", oneLine(log), parent, dir.Bookmark, base, marker)
	return nil
}

// cacheHolds is whether the cache name's clone, asked inside its own pod, holds the object id.
func (r *liveRig) cacheHolds(name, id string) (bool, error) {
	clone, err := workspace.FeedRepository(CacheRoot, r.env.repo)
	if err != nil {
		return false, err
	}
	out, err := r.kubectl("exec", name, "-c", cacheContainer, "--", "sh", "-c", `if git --git-dir="$1" cat-file -e "$2"; then echo held; else echo absent; fi`, "sh", clone, id)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(out) {
	case "held":
		return true, nil
	case "absent":
		return false, nil
	}
	return false, fmt.Errorf("asking the cache %s whether it holds %s answered %q", name, id, strings.TrimSpace(out))
}

// pushRunCommit puts branch on the run's repository at a new commit on parent that adds path
// holding content, written into the bare repository the fake GitHub serves it from, as an
// operator's push lands on GitHub, so nothing reaches the default branch. It returns the commit.
func (r *liveRig) pushRunCommit(branch, parent, path, content string) (string, error) {
	index := filepath.Join(r.env.scratch, "push-index")
	defer os.Remove(index)
	if _, err := r.bareGit(index, "", "read-tree", parent); err != nil {
		return "", err
	}
	blob, err := r.bareGit("", content, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	if _, err := r.bareGit(index, "", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path); err != nil {
		return "", err
	}
	tree, err := r.bareGit(index, "", "write-tree")
	if err != nil {
		return "", err
	}
	commit, err := r.bareGit("", "", "commit-tree", tree, "-p", parent, "-m", runCommitMessage(branch))
	if err != nil {
		return "", err
	}
	if _, err := r.bareGit("", "", "update-ref", "refs/heads/"+branch, commit, strings.Repeat("0", 40)); err != nil {
		return "", err
	}
	return commit, nil
}

// bareGit runs git with stdin against the bare repository the fake GitHub serves the run's
// repository from (liveEnv.repoBare), on the index file index where it is not empty, reading no
// configuration of the machine's, and returns its standard output, trimmed.
func (r *liveRig) bareGit(index, stdin string, args ...string) (string, error) {
	command := exec.CommandContext(r.ctx, "git", append([]string{"--git-dir=" + r.env.repoBare}, args...)...)
	command.Env = []string{
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + r.env.scratch, "PATH=" + os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=Stage 4a", "GIT_AUTHOR_EMAIL=stage4a@example.com",
		"GIT_COMMITTER_NAME=Stage 4a", "GIT_COMMITTER_EMAIL=stage4a@example.com",
	}
	if index != "" {
		command.Env = append(command.Env, "GIT_INDEX_FILE="+index)
	}
	command.Stdin = strings.NewReader(stdin)
	out, err := command.Output()
	if err != nil {
		var stderr string
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exit.Stderr))
		}
		return "", fmt.Errorf("git %s on %s: %v: %s", strings.Join(args, " "), r.env.repo, err, stderr)
	}
	return strings.TrimSpace(string(out)), nil
}

// assertCacheFed is cache-stream's evidence for c's pod: its workspace-fetch log's last line names
// the bytes it streamed from its repository's cache at the address the runtime gave it and the
// GitHub delta, under 1 MiB, and nothing was cloned from GitHub.
func (r *liveRig) assertCacheFed(c *liveClaim) error {
	log, err := r.fetchLog(SandboxName(c.token))
	if err != nil {
		return err
	}
	repo := r.claimRepository(c)
	address := r.rt.cacheURL(CacheSandboxName(r.env.project, repo))
	match := cacheStreamLine.FindStringSubmatch(log)
	if match == nil || match[2] != address || strings.Contains(log, "cloned "+repo.String()+" from GitHub") {
		return fmt.Errorf("%s's workspace-fetch log does not name the stream from the cache at %s alone: %s", c.name, address, strings.TrimSpace(log))
	}
	streamed, delta := printedCount(match[1]), printedCount(match[3])
	if streamed <= 0 || delta < 0 || delta >= 1<<20 {
		return fmt.Errorf("%s's feed came %d bytes from the cache and %d from GitHub, want some from the cache and under 1 MiB from GitHub: %s", c.name, streamed, delta, match[0])
	}
	note("operator", "%s's workspace-fetch log: %s", c.name, match[0])
	return nil
}

// printedCount is a byte count as workspace-init fetch prints it, digits grouped by commas; -1 when
// it is no such count.
func printedCount(printed string) int64 {
	n, err := strconv.ParseInt(strings.ReplaceAll(printed, ",", ""), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// awaitCacheReady waits for the run's repository's cache to be Ready as the runtime's stores show
// it, which is what an issue pod's launch reads.
func (r *liveRig) awaitCacheReady() (CacheStatus, error) {
	var status CacheStatus
	err := r.poll(liveRunningLimit, "the repository cache of "+r.env.repo.String()+" to be Ready", func() (bool, error) {
		var found bool
		status, found = r.rt.RepositoryCache(r.env.repo)
		return found && status.Ready, nil
	})
	return status, err
}

// fetchLog is the workspace-fetch container's log of the pod name.
func (r *liveRig) fetchLog(name string) (string, error) {
	out, err := r.kubectl("logs", name, "-c", fetchContainer)
	return out, err
}

// githubHead is the commit the fake GitHub names for branch of the run's repository: the ref its
// bare repository holds.
func (r *liveRig) githubHead(branch string) (string, error) {
	return r.bareGit("", "", "rev-parse", "--verify", "refs/heads/"+branch)
}
