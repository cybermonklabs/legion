package launcher

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The sweep's deletions, made by a removal that follows no symbolic link, where they see no more
// than a role sees.
//
// A handle bounds what the trimmer itself reaches: it names an entry by its own name in the
// directory the listing opened, which is the one read this kernel resolves by the descriptor
// alone (volumedir.go). The removal of an entry is a walk of its own below that name, and the
// one the deleter made before this, Root.RemoveAll, unlinks a name, and on EISDIR opens it again
// with the raw `openat(O_NOFOLLOW|O_DIRECTORY)` this kernel follows a link under, and recurses
// (os/removeall_at.go, go1.27.3) — it re-opens that name once more after every 1024 entries it
// has read. A role that swaps a subdirectory for a link in either window — between the unlink
// and the open that follows it, or between two batches — has the recursion delete the link's
// target: measured against the uncontained code on 2026-11-12, kernel 4.19.0-gvisor, under
// go1.27.3, where the swap landed in the batch window in four trials of four and every one of
// them deleted a file of another role's private directory, and again on 2026-11-14 with
// Root.RemoveAll called in a test process, where the first trial deleted every bait
// (TestTheDeletersRemovalFollowsNoLinkWithNoContainment, with followingRemoval in place of
// removeEntryAt).
//
// So the first bound is the removal itself: removeEntryAt (volumedir_linux.go) walks the entry
// bottom up through descriptors, opening each directory as one element of the one it is in and
// unlinking each name from its parent's descriptor, and a name that is a link is unlinked as a
// link and never opened. Raced the same way in a test process with no containment, the swap
// landed inside the removal in 80 trials of 80 and no bait was touched, on 2026-11-14, kernel
// 4.19.0-gvisor, under go1.27.3 (TestTheDeletersRemovalFollowsNoLinkWithNoContainment).
//
// The second bound is where the removal runs: in a mount namespace of its own, walked from the
// same list and by the same walk that build a role's (RoleCover.replaced and eachReplaced,
// roleinit.go), each with a callback of its own: every path a role's own namespace replaces with
// something of the role's is an empty filesystem of this child's instead, with a procfs of the
// child's own PID namespace where a role is given one and the child's working directory at the
// root of its own namespace (containDeletions). /legion is left as the container mounted it,
// since that is where the deletions are made. A link a removal did follow would then reach only
// what the role that left it could delete itself, as the same uid. The cases that hold this
// bound race a deleter that removes with Root.RemoveAll (useFollowingContainedDeleter), since a
// removal that follows no link reaches no cover at all: so raced, with the home's cover taken
// out of containDeletions and nothing else changed, the swap case deleted the bait under the home
// at that path's trial, on 2026-11-14, kernel 4.19.0-gvisor, under go1.27.3.
//
// What that bounds, exactly. All six roles of an issue pod run as one uid — the worker image's
// unprivileged user, roleUID — so a role can already delete anything of /legion it can reach by
// a path of its own, the other roles' caches and workspaces included. Containment takes none of
// that away and claims to take none of it away: what a followed link reaches inside the
// namespace is what the role that left it could have deleted itself, and the sweep's own guards
// — the handles, the kinds, the ages, the used-path set — are what keep an ordinary sweep off a
// role's work.
//
// What it removes is the reach into what the container holds for the roles and no role holds
// itself. Every directory where a role's own view differs from the container's is covered here,
// so what a link reaches in this child is what the role that left it reaches by that path, or
// nothing at all:
//
//   - Another role's credentials: its private, state and agent-secrets directories and its
//     launcher token, which live under /var/run/legion/roles, and the fixed root's own mount
//     points, where a role's are bound.
//   - What every role starts from: the pod's shared config home, which the container mounts
//     under /var/run/legion/home and which each role's own config home is copied from at that
//     role's first start in the pod (ownConfigHome), and /home/legion, the lower layer of every
//     role's home overlay.
//   - What the container keeps at every path a role is given its own of: its /tmp and /var/tmp,
//     its lock directories and POSIX shared memory, its pseudo-terminals and message queues,
//     and its /proc.
//
// Six of those paths were measured reached, each with the swap case below run against a cover
// with that one path taken out of the list and nothing else changed, with the deleter removing
// with Root.RemoveAll: it landed in four trials of four and deleted the bait under that path in
// each, on 2026-11-12, kernel 4.19.0-gvisor, under go1.27.3 — the roles directory, the home
// mounts directory, the home, the fixed root's private mount point, the container's /tmp and
// its /proc (TestASubdirectorySwappedForALinkWhileARemovalRecursesReachesNoOtherRolesFiles,
// where the image's own paths are a tree of the case's own, since nothing can plant a file in
// the machine's /proc). The rest are covered because a role's namespace replaces them. That case
// aims its link at every path of the list, each by its own name but /proc, so the recursion opens
// what the deleter is shown there rather than stopping at the cover's mount point, and it reads
// every bait back after each swap: a cover dropped from the list, or one that shows the
// container's own files at its path, is that path's bait deleted and named. At /proc the link
// names a directory inside it instead (replacedBait.within, volumedeletecover_test.go): the
// deleter's /proc is a procfs of its own, whose /proc/1/root and /proc/1/cwd name the deleter's
// own root, so a removal let into it would go on into all the deleter sees. That the deleter
// still deletes what the sweep lists with its own procfs over the image's /proc is
// TestTheDeleterOverTheImagesOwnPathsDeletesWhatTheSweepLists. The fixed root's private mount
// point bound onto itself in place of its cover turned the swap case red at that path's own
// trial, on 2026-11-13, kernel 4.19.0-gvisor, under go1.27.3.
//
// Two were measured against the deleter before this change rather than against a cover of its
// own: the container's /tmp, and the launcher's working directory reached through
// /proc/self/cwd, each in one trial of one (INFRA-2009, review 9, on 2026-11-12). The second is
// what the chdir in containDeletions ends, and the same swap with that chdir taken out deleted
// the bait under the launcher's working directory in four trials of four, on the same day and
// kernel (TestTheDeletersInheritedWorkingDirectoryIsNoWayPastACover).
//
// What the child is given as the container holds it, and what that leaves:
//
//   - /legion, which is what the sweep deletes in. A link to it reaches what the role that left
//     it reaches there, which is the bound above and nothing narrower.
//   - Every path a role's namespace leaves as the container's too — the image's own files, the
//     directories the covered paths sit in, /sys, the rest of /dev and /run. The child reads
//     none of them, and what a link to one of them reaches is again what the role that left it
//     reaches by the same path: the child holds no capability outside its own user namespace,
//     so what it can unlink anywhere is what the role uid can unlink.
//   - A name that is not a directory, the image's /dev/ptmx among them: both removals unlink such
//     a name as the name it is and never recurse through it, so a link to one costs the link and
//     nothing else.
//
// The shape: one child per sweep, started once the listing has opened every directory it will
// delete out of and ended with the sweep. A child per entry would be a fork and an exec per
// deletion, tens of thousands of them over one sweep of the measured pod's caches; a child for
// the trimmer's whole life would have to be handed each sweep's descriptors as it ran, which is
// a socket and SCM_RIGHTS for a process that lives between two sweeps holding descriptors on
// directories nobody is deleting out of. One per sweep passes the descriptors the one way a
// process is given descriptors for nothing (exec), and the requests are names in them.
//
// There is no fallback. A deleter that cannot start, and one that dies while the sweep runs,
// each leave the sweep deleting nothing more: it says so in one line of the pod's log and ends
// with that reason in its report (sweep, volumetrim.go). Deleting the rest in the launcher's
// own view is the behaviour this file exists to end, and a sweep that skips a cycle costs the
// pod one interval of a volume that is already full.

// VolumeTrimDeleteSubcommand is `legion launcher`'s second hidden subcommand, the one the
// trimmer runs itself, as RoleInitSubcommand is the one the launcher runs for a role.
const VolumeTrimDeleteSubcommand = "volume-trim-delete"

// volumeRemoverGrace is how long the sweep waits for its deleter to end after it has closed the
// requests, before it kills it. The deleter answers EOF by returning, so the grace is only the
// time an exit takes; what it rules out is the trimmer's goroutine — the launcher's, in the
// process that is PID 1 of the roles container — waiting on a child for ever, which is the fault
// this change's other half exists to remove.
const volumeRemoverGrace = 5 * time.Second

// entryRemover removes one listed entry, by its own name in the directory the listing holds
// open. The sweep takes one for the whole of its deletions and closes it when it ends; a test
// that is not about containment gives the trimmer one of its own (trimmer.removals).
type entryRemover interface {
	// removeAll removes name from dir, whatever it holds. A removal the kernel refused is the
	// error it refused with; an error wrapping errRemoverGone is the remover itself, which no
	// later removal can be asked of.
	removeAll(dir *sweepDir, name string) error
	// process is the pid of the process the removals are made in, as the trimmer's /proc
	// numbers it. The used-path set leaves it out with the trimmer's own (readUsedPaths): it
	// holds the listing's directories for the whole sweep, and that is the sweep's own hold.
	process() int
	// close ends the remover and everything it holds.
	close()
}

// errRemoverGone is the remover rather than the removal: the sweep stops at that entry and keeps
// what it has deleted, as it does for a /proc read the kernel refused while it ran (sweep).
var errRemoverGone = errors.New("the sweep's deleter is gone")

// startRemover is what the pod's trimmer takes for every sweep (trimmer.removals): the contained
// deleter, answered as the interface the sweep holds it by. A sweep is given no other, and a
// start that fails is a sweep that deletes nothing (sweep, volumetrim.go). The deleter is the
// launcher's own child and starts through guard, the launcher's pidGuard, so PID 1's reaper
// leaves its exit to the remover's own wait (containedRemover.close).
func startRemover(guard *pidGuard) func(RoleCover, []*sweepDir) (entryRemover, error) {
	return func(cover RoleCover, dirs []*sweepDir) (entryRemover, error) {
		return startContainedRemover(guard, cover, dirs)
	}
}

// trimDeleteLaunch is the command line and the extra environment that run one sweep's deleter:
// this binary's own hidden subcommand, as roleInitLaunch runs role-init. A test replaces it with
// its re-entrant helper, since a test binary is not `legion`.
//
// The cover goes on the command line as role-init's own does and from the one table that writes
// both (RoleCover.flags), so the deleter is told the whole of what the trimmer was given rather
// than a part of it, and a directory added to the cover reaches this command line with it.
var trimDeleteLaunch = func(cover RoleCover) (argv, env []string, err error) {
	self, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("find this binary: %w", err)
	}
	return append([]string{self, "launcher", VolumeTrimDeleteSubcommand}, cover.flags()...), nil, nil
}

// containedRemover is one sweep's deleter: the child process, the pipe its requests go down, the
// pipe its answers come back on, and which descriptor of the child's each listed directory is.
type containedRemover struct {
	cmd *exec.Cmd
	// guard holds the child's pid out of PID 1's reaper from its start until close has read its
	// exit (pidGuard).
	guard    *pidGuard
	requests io.WriteCloser
	replies  *bufio.Reader
	// at is the child's descriptor number for each directory the listing opened. The
	// descriptors are passed at the exec and never again, so a directory the listing did not
	// open is a directory this remover cannot be asked about at all.
	at   map[*sweepDir]int
	said *saidBuffer // what the child wrote to its standard error, which a failure carries
	gone error       // the first failure of the remover itself, which every later call answers
}

// startContainedRemover starts the deleter for one sweep: the child in its own user, mount, PID
// and IPC namespaces (roleNamespaceAttr, the same clone a role generation is started with),
// holding the descriptor of every directory the listing opened and nothing else of the volume.
// It starts through guard, as the test database's script does (runTestPG).
func startContainedRemover(guard *pidGuard, cover RoleCover, dirs []*sweepDir) (*containedRemover, error) {
	attr, err := roleNamespaceAttr()
	if err != nil {
		return nil, err
	}
	argv, extra, err := trimDeleteLaunch(cover)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = mergeEnv(os.Environ(), extra)
	cmd.SysProcAttr = attr
	remover := &containedRemover{cmd: cmd, guard: guard, at: make(map[*sweepDir]int, len(dirs)), said: &saidBuffer{}}
	for _, dir := range dirs {
		if _, held := remover.at[dir]; held {
			continue
		}
		remover.at[dir] = len(cmd.ExtraFiles)
		cmd.ExtraFiles = append(cmd.ExtraFiles, dir.handle.file)
	}
	cmd.Stderr = remover.said
	if remover.requests, err = cmd.StdinPipe(); err != nil {
		return nil, fmt.Errorf("the sweep's deleter: %w", err)
	}
	replies, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("the sweep's deleter: %w", err)
	}
	remover.replies = bufio.NewReader(replies)
	if _, err := guard.start(cmd); err != nil {
		return nil, fmt.Errorf("start the sweep's deleter in a mount namespace of its own: %w", err)
	}
	return remover, nil
}

// removeAll asks the deleter for one entry and waits for its answer. One request is in flight at
// a time: the sweep deletes one entry before it reads the volume again and decides whether to
// delete another, so there is nothing for a second to overlap with.
func (r *containedRemover) removeAll(dir *sweepDir, name string) error {
	if r.gone != nil {
		return r.gone
	}
	at, held := r.at[dir]
	if !held {
		// A directory the listing did not open, which no sweep asks for: the deleter holds no
		// descriptor for it, and naming it by path is exactly what this change exists to stop.
		return fmt.Errorf("%s was not among the directories the listing opened", filepath.Join(dir.root.Path, dir.shard))
	}
	if _, err := fmt.Fprintf(r.requests, "%d %s\n", at, strconv.Quote(name)); err != nil {
		return r.ended(err)
	}
	line, err := r.replies.ReadString('\n')
	if err != nil {
		return r.ended(err)
	}
	answer, refusal, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
	switch answer {
	case "removed":
		return nil
	case "failed":
		said, err := strconv.Unquote(refusal)
		if err != nil {
			return r.ended(fmt.Errorf("it answered %q, which is no answer of this protocol", line))
		}
		return errors.New(said)
	default:
		return r.ended(fmt.Errorf("it answered %q, which is no answer of this protocol", line))
	}
}

// ended records the first failure of the remover itself and is what every call answers after it.
func (r *containedRemover) ended(err error) error {
	if r.gone == nil {
		r.gone = fmt.Errorf("%w: %v%s", errRemoverGone, err, r.said.trailing())
	}
	return r.gone
}

// process is the deleter child's pid, which the trimmer's /proc lists it under: the child's own
// PID namespace numbers it 1, and the trimmer's numbers it as the child of the trimmer it is.
func (r *containedRemover) process() int {
	return r.cmd.Process.Pid
}

// close ends the deleter: the requests pipe is closed, which the child reads as the end of its
// work, and it is killed if it has not gone when the grace is over. The guard forgets its pid
// once this wait has read its exit.
func (r *containedRemover) close() {
	_ = r.requests.Close()
	ended := make(chan error, 1)
	go func() { ended <- r.cmd.Wait() }()
	select {
	case <-ended:
	case <-time.After(volumeRemoverGrace):
		_ = r.cmd.Process.Kill()
		<-ended
	}
	r.guard.done(r.cmd.Process.Pid)
}

// VolumeTrimDelete is `legion launcher volume-trim-delete`'s body: the containment, then one
// removal per request until the requests end.
//
// Every directory it removes from is a descriptor it was handed at the exec, and every request
// names one by its number among them and an entry by its own name in it: it resolves no path of
// the volume at all. The paths it does resolve are the ones its namespace replaces and its own
// /proc/self/fd, where a descriptor it was handed is opened as the directory a removal is made
// from (removeOneRequest). It refuses to run anywhere but as the init of a PID namespace of its
// own, which is what the trimmer starts it as: run by hand it holds no descriptor of the volume,
// and the refusal says so rather than leaving a process that has covered nothing reading
// requests.
//
// The cover it is given is the one role-init is given, whole (RoleCover): the trimmer passes it
// as the launcher built it, so that a role cannot have the deletions reach what no role reaches.
func VolumeTrimDelete(cover RoleCover, requests io.Reader, replies io.Writer) error {
	if err := cover.validate("volume-trim-delete"); err != nil {
		return err
	}
	if os.Getpid() != 1 {
		return errors.New("volume-trim-delete: not the init of a PID namespace of its own")
	}
	if err := containDeletions(cover); err != nil {
		return fmt.Errorf("volume-trim-delete: %w", err)
	}
	// One open directory per descriptor, made at the first request that names it: a sweep
	// deletes many entries out of one directory. The directories this process opened are closed
	// when the requests end; the descriptors the trimmer passed are not, and go with this
	// process, which ends with the sweep (removeOneRequest).
	dirs := map[int]*os.File{}
	defer func() {
		for _, dir := range dirs {
			_ = dir.Close()
		}
	}()
	// One buffer every removal reads its directories' names into, for the whole of the
	// requests (removeEntryAt).
	names := make([]byte, volumeRemovalNamesBytes)
	reader := bufio.NewReader(requests)
	for {
		line, err := reader.ReadString('\n')
		if line == "" {
			if err == nil || errors.Is(err, io.EOF) {
				return nil // the sweep is over and closed the pipe
			}
			return fmt.Errorf("volume-trim-delete: read a request: %w", err)
		}
		answer := "removed"
		if err := removeOneRequest(dirs, strings.TrimSuffix(line, "\n"), names); err != nil {
			answer = "failed " + strconv.Quote(err.Error())
		}
		if _, err := fmt.Fprintln(replies, answer); err != nil {
			return fmt.Errorf("volume-trim-delete: answer a request: %w", err)
		}
	}
}

// removeOneRequest removes the entry one request names, out of the descriptor it names. names is
// the deleter's one buffer for the names of the directories a removal reads.
func removeOneRequest(dirs map[int]*os.File, request string, names []byte) error {
	number, quoted, found := strings.Cut(request, " ")
	if !found {
		return fmt.Errorf("the request %q names no entry", request)
	}
	at, err := strconv.Atoi(number)
	if err != nil {
		return fmt.Errorf("the request %q names no descriptor: %w", request, err)
	}
	name, err := strconv.Unquote(quoted)
	if err != nil {
		return fmt.Errorf("the request %q names no entry: %w", request, err)
	}
	if !isOneName(name) {
		return fmt.Errorf("%q is not one name in one directory", name)
	}
	dir, held := dirs[at]
	if !held {
		// The descriptors the trimmer passed begin after the standard three, in the order the
		// listing opened the directories.
		fd := volumeDeleteFirstFD + at
		if at < 0 || fd < volumeDeleteFirstFD {
			return fmt.Errorf("the request names the descriptor %d, which the trimmer passed none at", at)
		}
		// The same /proc/self/fd open the listing's own handles are made over
		// (rootOverDescriptor, volumedir.go): the descriptor names the directory the listing
		// opened, whatever has happened to its path since. It is the path that is opened and
		// never the descriptor itself, so nothing here holds the trimmer's descriptor as a file
		// that could be closed under this process and recycled onto another directory.
		if dir, err = openPassedDir(fd); err != nil {
			return err
		}
		dirs[at] = dir
	}
	return removeListedEntry(dir, name, names)
}

// openPassedDir opens the directory the trimmer passed at fd, through this process's own
// /proc/self/fd, and refuses a descriptor that is not a directory: the trimmer passes only
// directories, so anything else at that number is not one of them.
func openPassedDir(fd int) (*os.File, error) {
	dir, err := os.Open(descriptorPath(fd))
	if err != nil {
		return nil, fmt.Errorf("the directory at descriptor %d: %w", fd, err)
	}
	if info, err := dir.Stat(); err != nil || !info.IsDir() {
		_ = dir.Close()
		if err == nil {
			err = fmt.Errorf("it is %s", kindOf(info.Mode()))
		}
		return nil, fmt.Errorf("the directory at descriptor %d: %w", fd, err)
	}
	return dir, nil
}

// removeListedEntry is the removal every request is answered with: removeEntryAt, which follows
// no symbolic link (volumedir_linux.go). It is a variable for the containment cases alone, which
// give their deleter a removal that does follow one, so that what they read back is what the
// covers bound by themselves: useFollowingContainedDeleter has it set to followingRemoval in the
// deleter it starts (launchTestDeleter with follows, read by TestVolumeTrimDeleteHelper,
// volumedelete_test.go), and useContainedDeleter leaves it as the pod's deleter has it.
var removeListedEntry = removeEntryAt

// volumeRemovalNamesBytes is the size of the buffer a deleter reads directories' names into.
const volumeRemovalNamesBytes = 8192

// volumeDeleteFirstFD is where the directories the listing opened begin in the deleter's own
// descriptors: the three standard ones are the request pipe, the answer pipe and its standard
// error, so the first directory is the fourth descriptor (exec.Cmd.ExtraFiles).
const volumeDeleteFirstFD = 3

// saidBuffer is what the deleter wrote to its standard error, which the parent reads while the
// child is still running: exec copies into it from a goroutine of its own, so the two meet under
// one lock. It is this package's own and not the tests' lockedBuffer, which is an alias for a
// test helper's buffer (launcher_test.go).
type saidBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *saidBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// trailing is what the child has said so far, as one parenthesised line for an error to carry,
// and "" when it has said nothing.
func (b *saidBuffer) trailing() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	said := strings.TrimSpace(b.buf.String())
	if said == "" {
		return ""
	}
	return " (" + strings.ReplaceAll(said, "\n", "; ") + ")"
}
