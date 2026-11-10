package leakguard

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// InstallDir is the one directory a pod's copy of the shared public leak check lives in, and
// Install is what puts it there. It is under the issue's scratch, `/legion/scratch`
// (internal/runtime/sandbox's scratchRoot): the roles of a pod share that scratch — TMPDIR, the
// cache home and the data home sit directly under it and every role is told the same one — so one
// install serves all six roles, and it dies with the pod, as a copy of a check that publishes
// often should. The directory is this package's constant rather than the runtime's because the
// installer makes it and the runtime names it (sandbox.mainEnvironment sets PUBLIC_LEAK_CHECK to
// InstalledCheck): one string, so the path a role is told and the path the install writes cannot
// drift.
const InstallDir = "/legion/scratch/leak-check"

// InstalledCheck is the check inside that directory — the launcher, with the python file it runs
// and the fixtures beside it — which is what PUBLIC_LEAK_CHECK names in every role's container and
// so the first place Resolve looks.
const InstalledCheck = InstallDir + "/" + checkName

// tagFile is where an install records the tag it landed, inside the installed directory and
// beside the launcher: the file is renamed into place with the check itself, so the record and
// the files it describes land together. A dot file, because the check's own directory is
// unpacked from the repository and this is the one entry of the install that is not of it.
const tagFile = ".installed-tag"

// TagVariable is the name of the pin the worker image carries (`ENV LEGION_LEAK_CHECK_TAG`,
// packages/daemon/docker/worker.Dockerfile, from its `ARG LEAK_CHECK_TAG`): the tag of the check's
// repository this pod installs. It is read from the shim's own environment, so the version lives in
// the Dockerfile alone and this package holds no second copy of it.
const TagVariable = "LEGION_LEAK_CHECK_TAG"

// Where the shared check is published: a tag of a private repository, whose `leak-check` directory
// holds the launcher (checkName), the python file the launcher runs from beside itself, and the
// fixtures its own self-test reads. The whole directory is installed for that reason — a launcher
// without its neighbours is a check that cannot start.
const (
	checkOwner     = "theorem-labs"
	checkRepo      = "agent-infra"
	checkSlug      = checkOwner + "/" + checkRepo
	checkDirectory = "leak-check"
)

// installWhat is what a failed install is reported as, in the words a *SetupFailure's What takes.
const installWhat = "the pod's install of the shared public leak check"

// installBound is how long the whole install has: the lease wait, the fetch and the one
// verification run together. The fetch is one `gh api` of a 16 MB tarball, measured at 3.3 s from a
// pod on 2026-11-09, and a role that waits waits for another role's fetch, so this is wide enough
// for a slow node several times over and still ends a launch rather than hanging it: a shim stuck
// here would never start its agent, never register, and leave the daemon to time the pod out with
// nothing to read.
const installBound = 5 * time.Minute

// leasePoll is how often a role that found the lease held looks again. The lease is flock(2), which
// has no bounded blocking form in Go's standard library, so it is taken non-blocking and retried:
// an install is seconds, and a tenth of a second of latency on top of a sibling's fetch costs a
// waiting role nothing.
const leasePoll = 100 * time.Millisecond

// InstallOptions are the install's inputs and its two seams, each a field a test fills and the
// shim leaves alone (as Options' three are).
type InstallOptions struct {
	// Env is the environment the pin is read from and the gh of the fetch runs with: the shim's
	// own (shim.Config.Env). Empty is this process's own environment.
	Env []string
	// Dir is where the check is installed; empty is InstallDir.
	Dir string
	// Log receives the install's one line — what was installed, or that another role of the pod
	// had already installed it. Nil logs nothing.
	Log func(format string, args ...any)
}

func (o InstallOptions) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Install puts the pinned shared public leak check in place for a pod, once, under a lease every
// role of the pod takes, and answers the installed check's path and a *SetupFailure when it could
// not. A caller runs it before it starts anything that writes (shim.spawnOnce, before the harness
// and before any daemon frame is read), and the path it answers is what that caller tells the
// processes it starts (shim's leakCheckOfInstall), so the check installed and the check named are
// one answer rather than two constants a release can part.
//
// With no pin in env it installs nothing, answers "" and succeeds. That is the tmux runtime: a
// pane's shim is started by the same command but from a machine's own environment, which carries
// no LEGION_LEAK_CHECK_TAG, and an operator's machine carries the check through the check's own
// installer rather than through a copy Legion unpacks into a developer's home. Where such a
// machine carries none, every entry point of this package refuses with a setup failure naming the
// missing check, which is the right answer on a machine that writes to public repositories with
// no check.
//
// The steps are in this order, and the order is the point:
//
//  1. the lease, taken before anything is read or decided, so the six role shims of one pod — which
//     start together — fetch once between them;
//  2. the installed check and the tag beside it (tagFile), which a role that waited for the lease
//     finds already there and leaves alone when that tag is this pin: the directory is renamed
//     into place whole, so its being there means a verified install and not a fetch that stopped
//     half way. An install of another tag, which only a directory outliving a pin change has —
//     the controller's persistent volume — is installed over rather than kept;
//  3. the fetch and unpack into a staging directory beside the final one, never into it;
//  4. the verification, run on the staging copy, so a check that does not answer never becomes the
//     check this pod would have written with;
//  5. the rename, which is atomic within the directory's own filesystem, and the release.
//
// Every failure before the rename leaves the install path as it was and the staging directory
// removed.
func Install(ctx context.Context, opts InstallOptions) (string, error) {
	env := opts.Env
	if len(env) == 0 {
		env = os.Environ()
	}
	tag := value(env, TagVariable)
	if tag == "" {
		// A machine with no pin installs nothing: the tmux runtime, where the operator's own
		// machine carries the check through the check's own installer, and where it carries none
		// every entry point of this package refuses the write and names what is missing (see the
		// paragraph above).
		return "", nil
	}
	// The pin becomes a path of a GitHub API endpoint below. The worker image is its only setter —
	// an operator's pod env may not name it (sandbox's imageEnv) — so this refuses a build's
	// mistake rather than an attack, and refuses it before the fetch rather than sending it.
	if strings.ContainsAny(tag, "/?#& \t\n") {
		return "", &SetupFailure{What: installWhat, Why: fmt.Sprintf("%s=%q is no tag of the check's repository", TagVariable, tag)}
	}
	dir := opts.Dir
	if dir == "" {
		dir = InstallDir
	}
	ctx, cancel := context.WithTimeout(ctx, installBound)
	defer cancel()
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", &SetupFailure{What: installWhat, Why: "the install directory's parent could not be made", Err: err}
	}
	release, err := takeInstallLease(ctx, dir+".lock")
	if err != nil {
		return "", &SetupFailure{What: installWhat, Why: "the install lease could not be taken", Err: err}
	}
	defer release()

	check := filepath.Join(dir, checkName)
	installed, there := installedTag(dir)
	if there && installed == tag {
		opts.logf("the shared public leak check %s is already installed at %s", tag, check)
		return check, nil
	}
	// Beside the final directory, so the rename below is a rename within one filesystem, and
	// hidden, so a half-unpacked directory is never mistaken for the install by a reader looking
	// at the scratch.
	staging, err := os.MkdirTemp(parent, ".leak-check-install-")
	if err != nil {
		return "", &SetupFailure{What: installWhat, Why: "the staging directory could not be made", Err: err}
	}
	defer os.RemoveAll(staging)
	if err := fetchCheck(ctx, env, tag, staging); err != nil {
		return "", err
	}
	if err := verifyInstalled(ctx, env, filepath.Join(staging, checkName)); err != nil {
		return "", err
	}
	// The tag is written into the staging copy, so the rename lands the check and the record of
	// which pin it is in one step: a directory that is there is a verified install of the tag its
	// own file names, and never an install whose record is still to be written.
	if err := os.WriteFile(filepath.Join(staging, tagFile), []byte(tag+"\n"), 0o644); err != nil {
		return "", &SetupFailure{What: installWhat, Why: "the installed tag could not be recorded", Err: err}
	}
	// 0700 is what MkdirTemp makes. The roles of a pod run as one user, so the mode decides
	// nothing between them, but the install is read by every role and read-only to all of them.
	if err := os.Chmod(staging, 0o755); err != nil {
		return "", &SetupFailure{What: installWhat, Why: "the staged install could not be made readable", Err: err}
	}
	if there {
		// Another tag's install is in the way, and a rename onto a directory is refused — by
		// every filesystem for one that is not empty, and by gVisor, which a pod runs under, for
		// an empty one too. So it is moved aside, to a name beside the staging directory's and
		// so on its filesystem, and removed once the new one is in place rather than before it:
		// the path names a whole check at every moment but the one between the two renames, and
		// a second rename that fails puts the old check back.
		aside := staging + ".replaced"
		defer os.RemoveAll(aside)
		if err := os.Rename(dir, aside); err != nil {
			return "", &SetupFailure{What: installWhat, Why: "the install of " + installed + " at " + dir + " could not be moved aside", Err: err}
		}
		if err := os.Rename(staging, dir); err != nil {
			_ = os.Rename(aside, dir)
			return "", &SetupFailure{What: installWhat, Why: "the staged install could not be moved to " + dir, Err: err}
		}
		opts.logf("installed the shared public leak check %s at %s, over the install of %s that was there", tag, check, installed)
		return check, nil
	}
	if err := os.Rename(staging, dir); err != nil {
		return "", &SetupFailure{What: installWhat, Why: "the staged install could not be moved to " + dir, Err: err}
	}
	opts.logf("installed the shared public leak check %s at %s", tag, check)
	return check, nil
}

// installedTag is the tag of the install at dir and whether there is an install there at all: the
// check itself with the tag file beside it, which is what Install lands. A directory holding the
// check and no tag — an install an earlier release made, which recorded none — is an install of
// no tag this pin can match, so it is installed over once and carries its tag from then on.
func installedTag(dir string) (tag string, there bool) {
	if _, err := os.Stat(filepath.Join(dir, checkName)); err != nil {
		return "", false
	}
	recorded, err := os.ReadFile(filepath.Join(dir, tagFile))
	if err != nil {
		return "", true
	}
	return strings.TrimSpace(string(recorded)), true
}

// takeInstallLease holds path's flock(2) until release is called. flock is the lease a pod wants
// here: the kernel releases it when the holder's file descriptor goes, so a shim killed mid-install
// — a pod evicted, a role stopped — leaves no lease behind for the next role to wait out, which is
// the failure a lease file with a staleness rule has to work around (internal/workspace's
// acquireWarmLease, whose holder is a long build rather than a launch). It is taken non-blocking
// and retried every leasePoll, so ctx's end is answered rather than waited past.
func takeInstallLease(ctx context.Context, path string) (release func(), err error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(leasePoll):
		}
	}
}

// fetchCheck fetches the tarball of the check's repository at tag and unpacks its leak-check
// directory, and nothing else of that repository, into dir. The tarball is read as it arrives
// rather than written out first: it is 16 MB of a repository whose leak-check directory is about
// one, and the pod's disk is the issue's.
func fetchCheck(ctx context.Context, env []string, tag, dir string) error {
	endpoint := "repos/" + checkSlug + "/tarball/" + tag
	gh, configDir := fetchGh(env)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "api", endpoint)
	// procgroup before any other SysProcAttr field, as its doc requires.
	procgroup.Configure(cmd)
	cmd.Env = append(slices.Clone(env), "GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1")
	if configDir != "" {
		cmd.Env = append(cmd.Env, "GH_CONFIG_DIR="+configDir)
	}
	var failed strings.Builder
	cmd.Stderr = &failed
	body, err := cmd.StdoutPipe()
	if err != nil {
		return &SetupFailure{What: installWhat, Why: "gh could not be started", Err: err}
	}
	if err := cmd.Start(); err != nil {
		return &SetupFailure{What: installWhat, Why: "gh could not be started", Err: err}
	}
	files, unpacked := unpackCheck(body, dir)
	if unpacked != nil {
		// Nothing will read the rest of the body, so the fetch is ended rather than drained.
		cancel()
	}
	waited := cmd.Wait()
	switch status := cmd.ProcessState.ExitCode(); {
	case status > 0:
		// gh's own answer is the one a reader needs: a 404 of the tag, a credential that does not
		// reach the repository, a host that cannot be reached.
		return &SetupFailure{What: installWhat, Why: fmt.Sprintf("`gh api %s` exited %d", endpoint, status),
			Err: errors.New(strings.TrimSpace(failed.String()))}
	case unpacked != nil:
		return &SetupFailure{What: installWhat, Why: "the fetched tarball could not be unpacked", Err: unpacked}
	case waited != nil:
		return &SetupFailure{What: installWhat, Why: "`gh api " + endpoint + "` did not finish", Err: waited}
	case files == 0:
		return &SetupFailure{What: installWhat,
			Why: fmt.Sprintf("the tarball of %s at %s holds no %s directory", checkSlug, tag, checkDirectory)}
	}
	return nil
}

// fetchGh is the gh the fetch runs and the gh directory it reads its credential from.
//
// It is the real gh (LEGION_GH_PATH, which the runtime tells every role) rather than the `gh` first
// on a pod's PATH, which is the `legion gh` wrapper. The wrapper calls this package's guard before
// every call it makes (cmd/legion/gh.go), the guard resolves the shared check first, and the check
// is what this install is putting there — so a fetch through the wrapper would be refused by the
// very absence it is curing, and no pod would ever launch. Nothing the wrapper does is skipped
// otherwise: its other work is picking the gh directory whose token reaches the command's
// repository, which is what the directory below is, and the guard's work is reading a write, while
// this is a read of a private repository that publishes nothing of this one.
//
// The directory is picked as `legion gh` picks one, through the one helper both go to
// (ghconfig.RepositoryDir, which cmd/legion's repositoryGHConfigDir is the other caller of): the
// repository's own, whose token reaches that one repository, where the role's bundle holds it,
// and the owner's own otherwise. A bundle with neither, and a machine with no bundle at all,
// leaves GH_CONFIG_DIR as the environment has it — which on a machine with the pin and a plain
// gh is that gh's own configuration. That is this caller's own answer to a miss, and the one way
// it differs from `legion gh`, which refuses an owner its bundle holds no directory for.
func fetchGh(env []string) (gh, configDir string) {
	gh = value(env, "LEGION_GH_PATH")
	if gh == "" {
		gh = "gh"
		if path, ok := lookpath.On(value(env, "PATH"), "gh"); ok {
			gh = path
		}
	}
	owners := value(env, "LEGION_GH_OWNERS_DIR")
	if owners == "" {
		return gh, ""
	}
	return gh, ghconfig.RepositoryDir(owners, checkOwner, checkRepo)
}

// unpackCheck writes the leak-check directory of the gzipped tarball r carries into dir, and
// answers how many files it wrote. GitHub's tarball of a repository holds every member under one
// `<owner>-<repo>-<sha>/` directory, so the members this reads are `<that>/leak-check/…`, and it
// reads the whole of that directory: the launcher resolves the check beside itself and its self-test
// reads a fixture from there, so a file left out is an install that may not run.
//
// A member of that directory that is neither a directory nor a regular file, and one whose path
// does not stay inside it, fails the unpack rather than being skipped: this package has no use for
// either, and a tarball holding one is not the tarball this pin means.
func unpackCheck(r io.Reader, dir string) (int, error) {
	zipped, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	archive := tar.NewReader(zipped)
	files := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, err
		}
		rel, under := memberPath(header.Name)
		if !under || rel == "" {
			continue
		}
		if !filepath.IsLocal(rel) {
			return files, fmt.Errorf("the member %q is no path inside %s/", header.Name, checkDirectory)
		}
		target := filepath.Join(dir, rel)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			if err := writeMember(target, archive, header.FileInfo().Mode().Perm()); err != nil {
				return files, err
			}
			files++
		default:
			return files, fmt.Errorf("the member %q is neither a directory nor a regular file", header.Name)
		}
	}
}

// memberPath is a tarball member's path under the check's directory, and under false for every
// member of the rest of the repository. The empty path is that directory's own entry.
func memberPath(name string) (string, bool) {
	_, rest, nested := strings.Cut(name, "/")
	if !nested {
		return "", false
	}
	if rest == checkDirectory {
		return "", true
	}
	return strings.CutPrefix(rest, checkDirectory+"/")
}

func writeMember(path string, content io.Reader, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// verifyInstalled runs the freshly unpacked check once, before anything may use it, in the one
// shape that costs nothing and reads nothing of anyone's: `public-leak-check.sh --help`.
//
// What is read is what it printed, never its status. Measured at leak-check-v15: `--help` prints
// the launcher's own usage header — the line naming the shapes this package calls, `check-public-text`
// and `gh-guard` — and exits 1; a copy whose python file did not land beside the launcher prints
// `LEAK CHECK ERROR: … is missing, so the check could not start` and exits 2, as does one whose
// /usr/bin/python3 will not start. So an install that cannot name its own shapes is a failed
// install, and a check that answers nothing at all is the same failure: this pod would otherwise
// have run with a PUBLIC_LEAK_CHECK that refuses every write it is asked about.
func verifyInstalled(ctx context.Context, env []string, check string) error {
	cmd := exec.CommandContext(ctx, check, "--help")
	procgroup.Configure(cmd)
	cmd.Env = env
	var printed strings.Builder
	cmd.Stdout, cmd.Stderr = &printed, &printed
	err := procgroup.Err(cmd.Run())
	answer := printed.String()
	if err != nil {
		return &SetupFailure{What: installWhat, Why: "the installed check could not be run", Err: err}
	}
	for _, shape := range []string{"check-public-text", "gh-guard"} {
		if !strings.Contains(answer, shape) {
			return &SetupFailure{What: installWhat,
				Why: fmt.Sprintf("the installed check's own help names no %s shape, so it is no check this guard can call", shape),
				Err: printedError(answer)}
		}
	}
	return nil
}
