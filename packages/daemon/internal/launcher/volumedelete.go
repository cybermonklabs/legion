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

// The sweep's deletions, made where they see no more than a role sees.
//
// A handle bounds what the trimmer itself reaches: it names an entry by its own name in the
// directory the listing opened, which is the one read this kernel resolves by the descriptor
// alone (volumedir.go). What no handle bounds is what the kernel does inside one call the
// trimmer makes and does not drive: Root.RemoveAll unlinks a name, and on EISDIR opens it again
// with the raw `openat(O_NOFOLLOW|O_DIRECTORY)` this kernel follows a link under, and recurses
// (os/removeall_at.go, go1.27.3) — it re-opens that name once more after every 1024 entries it
// has read. A role that swaps a subdirectory for a link in either window — between the unlink
// and the open that follows it, or between two batches — has the recursion delete the link's
// target: measured against the uncontained code on 2026-11-12, kernel 4.19.0-gvisor, under
// go1.27.3, where the swap landed in the batch window in four trials of four and every one of
// them deleted a file of another role's private directory
// (TestASubdirectorySwappedForALinkWhileARemovalRecursesReachesNoOtherRolesFiles).
//
// So the deletions are made in a mount namespace of their own, with /var/run/legion/roles
// covered by an empty filesystem as each role's own namespace covers it (roleinit_linux.go) and
// /legion as the container mounted it. A link the recursion follows then reaches only what the
// role that left it could delete itself, as the same uid.
//
// What that bounds, exactly. All six roles of an issue pod run as one uid — the worker image's
// unprivileged user, roleUID — so a role can already delete anything of /legion it can reach by
// a path of its own, the other roles' caches and workspaces included. Containment takes none of
// that away and claims to take none of it away: what a followed link reaches inside the
// namespace is what the role that left it could have deleted itself, and the sweep's own guards
// — the handles, the kinds, the ages, the used-path set — are what keep an ordinary sweep off a
// role's work.
//
// What it removes is one reach and one only: the reach across the role-isolation boundary, into
// the private, state and agent-secrets directories of another role and that role's launcher
// token. Those live under /var/run/legion/roles, which the container's mount namespace shows to
// the launcher and no role's own namespace shows at all (prepareRoleNamespace covers it with an
// empty tmpfs), so a link the deleter follows there lands in the cover and finds nothing. This
// says nothing about paths outside that boundary that no one has looked for: it is the class
// the cover covers, not a proof that the cover is the only thing worth covering.
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
	// close ends the remover and everything it holds.
	close()
}

// errRemoverGone is the remover rather than the removal: the sweep stops at that entry and keeps
// what it has deleted, as it does for a /proc read the kernel refused while it ran (sweep).
var errRemoverGone = errors.New("the sweep's deleter is gone")

// startRemover is what the pod's trimmer takes for every sweep (trimmer.removals): the contained
// deleter, answered as the interface the sweep holds it by. A sweep is given no other, and a
// start that fails is a sweep that deletes nothing (sweep, volumetrim.go).
func startRemover(rolesDir string, dirs []*sweepDir) (entryRemover, error) {
	return startContainedRemover(rolesDir, dirs)
}

// trimDeleteLaunch is the command line and the extra environment that run one sweep's deleter:
// this binary's own hidden subcommand, as roleInitLaunch runs role-init. A test replaces it with
// its re-entrant helper, since a test binary is not `legion`.
var trimDeleteLaunch = func(rolesDir string) (argv, env []string, err error) {
	self, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("find this binary: %w", err)
	}
	return []string{self, "launcher", VolumeTrimDeleteSubcommand, "--roles-dir", rolesDir}, nil, nil
}

// containedRemover is one sweep's deleter: the child process, the pipe its requests go down, the
// pipe its answers come back on, and which descriptor of the child's each listed directory is.
type containedRemover struct {
	cmd      *exec.Cmd
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
func startContainedRemover(rolesDir string, dirs []*sweepDir) (*containedRemover, error) {
	attr, err := roleNamespaceAttr()
	if err != nil {
		return nil, err
	}
	argv, extra, err := trimDeleteLaunch(rolesDir)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = mergeEnv(os.Environ(), extra)
	cmd.SysProcAttr = attr
	remover := &containedRemover{cmd: cmd, at: make(map[*sweepDir]int, len(dirs)), said: &saidBuffer{}}
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
	if err := cmd.Start(); err != nil {
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

// close ends the deleter: the requests pipe is closed, which the child reads as the end of its
// work, and it is killed if it has not gone when the grace is over.
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
}

// VolumeTrimDelete is `legion launcher volume-trim-delete`'s body: the containment, then one
// removal per request until the requests end.
//
// Every directory it removes from is a descriptor it was handed at the exec, and every request
// names one by its number among them and an entry by its own name in it: it resolves no path of
// the volume at all, and the only path it reads is the roles directory it covers. It refuses to
// run anywhere but as the init of a PID namespace of its own, which is what the trimmer starts
// it as: run by hand it holds no descriptor of the volume, and the refusal says so rather than
// leaving a process that has covered nothing reading requests.
func VolumeTrimDelete(rolesDir string, requests io.Reader, replies io.Writer) error {
	if !filepath.IsAbs(rolesDir) {
		return fmt.Errorf("volume-trim-delete: roles directory %q is not absolute", rolesDir)
	}
	if os.Getpid() != 1 {
		return errors.New("volume-trim-delete: not the init of a PID namespace of its own")
	}
	if err := containDeletions(rolesDir); err != nil {
		return fmt.Errorf("volume-trim-delete: %w", err)
	}
	// One Root per descriptor, made at the first request that names it: a sweep deletes many
	// entries out of one directory, and each Root is a descriptor of its own.
	roots := map[int]*os.Root{}
	defer func() {
		for _, root := range roots {
			_ = root.Close()
		}
	}()
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
		if err := removeOneRequest(roots, strings.TrimSuffix(line, "\n")); err != nil {
			answer = "failed " + strconv.Quote(err.Error())
		}
		if _, err := fmt.Fprintln(replies, answer); err != nil {
			return fmt.Errorf("volume-trim-delete: answer a request: %w", err)
		}
	}
}

// removeOneRequest removes the entry one request names, out of the descriptor it names.
func removeOneRequest(roots map[int]*os.Root, request string) error {
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
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("%q is not one name in one directory", name)
	}
	root, held := roots[at]
	if !held {
		// The descriptors the trimmer passed begin after the standard three, in the order the
		// listing opened the directories.
		fd := volumeDeleteFirstFD + at
		if at < 0 || fd < volumeDeleteFirstFD {
			return fmt.Errorf("the request names the descriptor %d, which the trimmer passed none at", at)
		}
		if root, err = os.OpenRoot(descriptorPath(fd)); err != nil {
			return fmt.Errorf("the directory at descriptor %d: %w", fd, err)
		}
		roots[at] = root
	}
	return root.RemoveAll(name)
}

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
