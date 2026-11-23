package leakguard

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// InstallDir is the one directory a pod's copy of the shared public leak check lives in. The
// daemon reads the check from its repository and pushes it to the pod's launcher, which installs
// it here once for the pod, before any role starts (InstallServed); each role's shim only reads
// the install (Installed) and refuses to start an agent where it is not the pin's. It is under the
// issue's scratch, `/legion/scratch` (internal/runtime/sandbox's scratchRoot): the roles of a pod
// share that scratch — TMPDIR, the cache home and the data home sit directly under it and every
// role is told the same one — so one install serves all six roles, and it dies with the pod, as a
// copy of a check that publishes often should. The directory is this package's constant rather
// than the runtime's because the install writes it and the runtime names it
// (sandbox.mainEnvironment sets PUBLIC_LEAK_CHECK to InstalledCheck): one string, so the path a
// role is told and the path the install writes cannot drift.
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
// repository this pod runs. The pod's launcher reads it from its own environment and names it to
// the daemon, which serves that tag; each role's shim reads it from its own and starts no agent
// unless the install is of it (Installed). So the version lives in the Dockerfile alone and this
// package holds no second copy of it.
const TagVariable = "LEGION_LEAK_CHECK_TAG"

// Where the shared check is published: a tag of the repository the deployment names
// (Setting.Repository), whose `leak-check` directory holds the launcher (checkName), the python
// file the launcher runs from beside itself, and the fixtures its own self-test reads. The whole
// directory is what the daemon packs and the launcher installs for that reason — a launcher
// without its neighbours is a check that cannot start.
const checkDirectory = "leak-check"

// installWhat is what a failed read of the install is reported as, in the words a
// *SetupFailure's What takes.
const installWhat = "the pod's install of the shared public leak check"

// installBound is how long the launcher's whole install has: the unpack, the one verification run
// and the renames together. The body is in memory by then, so this is wide enough for a slow node
// several times over and still ends the install rather than hanging the launcher's answer.
const installBound = 5 * time.Minute

// The read limits of PackTarballManifest. The tarball of the check's repository was 16 MB at
// leak-check-v15 and its leak-check directory about one, so each is far above what the repository
// holds and still bounds what one read may take of the daemon's memory.
const (
	maxTarballBytes = 256 << 20
	maxPackedBytes  = 16 << 20
)

// errOverLimit is what a cappedBuffer answers once its limit is passed; the caller reads that it
// was passed from the buffer's own over.
var errOverLimit = errors.New("over the read limit")

// PackTarballManifest reads a GitHub tarball of the check's repository (gzip, then tar), and
// answers its leak-check directory alone, packed into a new gzipped tarball whose member paths are
// the directory's own — no repository prefix and no leading `leak-check/` — with each file's bytes
// and mode as the tarball has them, and the manifest digest (Manifest) of the exact bytes of the
// manifest files it packs. That body is what the daemon serves to a pod's launcher
// (InstallServed). Each manifest file streams through its sha256 as it is written into the body,
// so the source need not unpack or copy the body it already holds.
//
// It refuses a member of that directory that is neither a directory nor a regular file, a member
// whose path does not stay inside it, a tarball with no such directory, one whose directory holds
// no regular file or misses a file of the manifest, a manifest file past manifestFileLimit or
// there twice, a tarball over maxTarballBytes and a directory that packs over maxPackedBytes.
// Each error is a predicate of the tarball, written to follow
// `the tarball of <the repository> at <tag> ` in the cause sentence the daemon builds of it
// (Cause), and comes with no body and no digest.
func PackTarballManifest(r io.Reader) ([]byte, string, error) {
	in := &io.LimitedReader{R: r, N: maxTarballBytes + 1}
	out := &cappedBuffer{left: maxPackedBytes}
	sums := make(map[string][sha256.Size]byte, len(ManifestFiles))
	files, found, err := pack(in, out, sums)
	switch {
	case in.N <= 0:
		return nil, "", fmt.Errorf("is over the %d MiB this daemon reads of one", maxTarballBytes>>20)
	case out.over:
		return nil, "", fmt.Errorf("holds a %s directory that packs over the %d MiB this daemon serves", checkDirectory, maxPackedBytes>>20)
	case err != nil:
		return nil, "", err
	case !found:
		return nil, "", fmt.Errorf("holds no %s directory", checkDirectory)
	case files == 0:
		return nil, "", fmt.Errorf("holds a %s directory with no file", checkDirectory)
	}
	digest, err := manifestFromSHA256(func(name string) ([sha256.Size]byte, error) {
		sum, held := sums[name]
		if !held {
			return sum, fs.ErrNotExist
		}
		return sum, nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("holds a %s directory without a file of its manifest: %w", checkDirectory, err)
	}
	return out.buf.Bytes(), digest, nil
}

func isManifestFile(name string) bool {
	for _, manifest := range ManifestFiles {
		if name == manifest {
			return true
		}
	}
	return false
}

// pack copies the leak-check directory of the tarball r carries into a gzipped tarball written to
// w, and answers how many files it wrote and whether the directory was there at all. Each manifest
// file is hashed into sums, by its name, as its bytes stream into the packed tarball. GitHub's
// tarball of a repository holds every member under one `<owner>-<repo>-<sha>/` directory, so the
// members this reads are `<that>/leak-check/…` (memberPath).
func pack(r io.Reader, w io.Writer, sums map[string][sha256.Size]byte) (files int, found bool, err error) {
	zipped, err := gzip.NewReader(r)
	if err != nil {
		return 0, false, fmt.Errorf("could not be read: %w", err)
	}
	archive := tar.NewReader(zipped)
	out := gzip.NewWriter(w)
	packed := tar.NewWriter(out)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, false, fmt.Errorf("could not be read: %w", err)
		}
		rel, under := memberPath(header.Name)
		if !under {
			continue
		}
		found = true
		if rel == "" {
			continue
		}
		if !filepath.IsLocal(rel) {
			return 0, false, fmt.Errorf("holds the member %q, which is no path inside %s/", header.Name, checkDirectory)
		}
		mode := int64(header.FileInfo().Mode().Perm())
		switch header.Typeflag {
		case tar.TypeDir:
			err = packed.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.TrimSuffix(rel, "/") + "/", Mode: mode, ModTime: header.ModTime})
		case tar.TypeReg:
			manifest := isManifestFile(rel)
			if manifest {
				if header.Size > manifestFileLimit {
					return 0, false, fmt.Errorf("holds the manifest file %q, which is over the %d MiB read limit", rel, manifestFileLimit>>20)
				}
				if _, twice := sums[rel]; twice {
					return 0, false, fmt.Errorf("holds the manifest file %q more than once", rel)
				}
			}
			err = packed.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: mode, Size: header.Size, ModTime: header.ModTime})
			if err == nil {
				if !manifest {
					_, err = io.Copy(packed, archive)
				} else {
					sum := sha256.New()
					if _, err = io.Copy(io.MultiWriter(packed, sum), archive); err == nil {
						sums[rel] = [sha256.Size]byte(sum.Sum(nil))
					}
				}
			}
			files++
		default:
			return 0, false, fmt.Errorf("holds the member %q, which is neither a directory nor a regular file", header.Name)
		}
		if err != nil {
			return 0, false, fmt.Errorf("could not be packed: %w", err)
		}
	}
	if err := packed.Close(); err != nil {
		return 0, false, fmt.Errorf("could not be packed: %w", err)
	}
	if err := out.Close(); err != nil {
		return 0, false, fmt.Errorf("could not be packed: %w", err)
	}
	return files, found, nil
}

// cappedBuffer holds what is written to it and answers errOverLimit, setting over, for a write
// that would take it past left bytes.
type cappedBuffer struct {
	buf  bytes.Buffer
	left int
	over bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.over || len(p) > c.left {
		c.over = true
		return 0, errOverLimit
	}
	c.left -= len(p)
	return c.buf.Write(p)
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

// ServedInstall is what InstallServed installs: the body the daemon served
// (PackTarballManifest's), the tag it is of, and where it goes.
type ServedInstall struct {
	// Dir is where the check is installed, which the caller names: InstallServed refuses an empty
	// one.
	Dir string
	// Tag is the tag the body is of, which the install records beside the check (tagFile).
	Tag string
	// Body is the packed check: a gzipped tarball of the leak-check directory's own paths.
	Body []byte
	// Log receives the install's one line where it replaced what Dir held. Nil logs nothing.
	Log func(format string, args ...any)
	// Start starts the install's one run of the check (verifyServed), and is required: the pod's
	// launcher passes the starter that holds the run out of its PID 1 reaper, and any other caller
	// procgroup.NoReaper (procgroup.Starter says why there is no default).
	Start procgroup.Starter
}

func (in ServedInstall) logf(format string, args ...any) {
	if in.Log != nil {
		in.Log(format, args...)
	}
}

// InstallServed installs the check the daemon served into in.Dir. The pod's launcher runs it once
// for the pod, before any role starts; every role's shim then reads the install (Installed).
//
// The steps are in this order, and the order is the point:
//
//  1. the unpack into a staging directory beside the final one, never into it;
//  2. the tag, written into the staging copy, so the rename lands the check and the record of
//     which pin it is in one step;
//  3. the verification, run on the staging copy, so a check that does not answer never becomes
//     the check this pod writes with;
//  4. whatever the directory held moved aside, the staging copy renamed into place — atomic within
//     the directory's own filesystem — and what was moved aside removed.
//
// Every failure leaves the directory as it was and the staging directory removed. Each error is
// the pod's own sentence of what happened, which the launcher hands the daemon as the cause
// (Cause). The whole install runs under installBound.
func InstallServed(ctx context.Context, in ServedInstall) error {
	if !shimwire.ValidLeakCheckTag(in.Tag) {
		return fmt.Errorf("the pod's launcher was served the tag %q, which is not of the published shape leak-check-v<n>", in.Tag)
	}
	if in.Dir == "" {
		return errors.New("the pod's launcher named no directory to install the check into")
	}
	if in.Start == nil {
		return errors.New("the pod's launcher named no starter for the check's run")
	}
	dir := in.Dir
	ctx, cancel := context.WithTimeout(ctx, installBound)
	defer cancel()
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("the install directory's parent could not be made: %w", err)
	}
	// Beside the final directory, so the rename below is a rename within one filesystem, and
	// hidden, so a half-unpacked directory is never mistaken for the install by a reader looking
	// at the scratch.
	staging, err := os.MkdirTemp(parent, ".leak-check-install-")
	if err != nil {
		return fmt.Errorf("the staging directory could not be made: %w", err)
	}
	// The parent is under the scratch every role writes, so a role can swap it for a fifo: the
	// staging directory, and what is moved aside below, are removed without a plain open of it.
	defer func() { _ = rolefile.RemoveTree(staging) }()
	files, err := unpackServed(in.Body, staging)
	if err == nil && files == 0 {
		err = errors.New("it holds no file")
	}
	if err != nil {
		return fmt.Errorf("the pod's launcher could not unpack the check the daemon served: %w", err)
	}
	// The tag is read by every role as the check's files are, so it is written as a member is
	// (writeMember): a new file whatever is at its path, of a mode set past the umask.
	if err := writeMember(filepath.Join(staging, tagFile), strings.NewReader(in.Tag+"\n"), 0o644); err != nil {
		return fmt.Errorf("the installed tag could not be recorded: %w", err)
	}
	if err := verifyServed(ctx, in.Start, filepath.Join(staging, checkName), parent); err != nil {
		return err
	}
	// 0700 is what MkdirTemp makes; 0755 lets every reader in. The roles of a pod run as one user,
	// which owns the install, so the mode decides nothing between them: every role can write the
	// install as well as read it. That is why a role's guard under the deployment's digest runs a
	// private copy of the bytes it hashed rather than the install itself (Guard.sharedCheck).
	if err := os.Chmod(staging, 0o755); err != nil {
		return fmt.Errorf("the staged install could not be made readable: %w", err)
	}
	if _, err := os.Lstat(dir); err == nil {
		// Something is in the way — an install of another tag, one of the same tag a restarted
		// launcher installs over, or anything else — and a rename onto a directory is refused: by
		// every filesystem for one that is not empty, and by gVisor, which a pod runs under, for
		// an empty one too. So it is moved aside, to a name beside the staging directory's and so
		// on its filesystem, and removed once the new one is in place rather than before it: the
		// path names a whole check at every moment but the one between the two renames, and a
		// second rename that fails puts the old one back.
		held := "what " + dir + " held"
		if tag, there, _ := installedTag(dir); there && tag != "" {
			held = "the install of " + tag + " at " + dir
		}
		aside := staging + ".replaced"
		defer func() { _ = rolefile.RemoveTree(aside) }()
		if err := os.Rename(dir, aside); err != nil {
			return fmt.Errorf("%s could not be moved aside: %w", held, err)
		}
		if err := os.Rename(staging, dir); err != nil {
			_ = os.Rename(aside, dir)
			return fmt.Errorf("the staged install could not be moved to %s: %w", dir, err)
		}
		in.logf("the install of %s replaced %s", in.Tag, held)
		return nil
	}
	if err := os.Rename(staging, dir); err != nil {
		return fmt.Errorf("the staged install could not be moved to %s: %w", dir, err)
	}
	return nil
}

// unpackServed writes the served body into dir and answers how many files it wrote. The body
// carries the leak-check directory's own paths, so every member is written at its own path under
// dir. A member whose path does not stay inside dir and one that is neither a directory nor a
// regular file fail the unpack rather than being skipped, and no file is written over another
// (writeMember's O_EXCL): a body holding either is not the body PackTarballManifest makes.
func unpackServed(body []byte, dir string) (int, error) {
	zipped, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	archive := tar.NewReader(zipped)
	files := 0
	// made are the directories mkdirShared has made, which no later member makes again.
	made := map[string]bool{}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return files, err
		}
		if !filepath.IsLocal(header.Name) {
			return files, fmt.Errorf("the member %q is no path inside the check's directory", header.Name)
		}
		target := filepath.Join(dir, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := mkdirShared(dir, target, made); err != nil {
				return files, err
			}
		case tar.TypeReg:
			if err := mkdirShared(dir, filepath.Dir(target), made); err != nil {
				return files, err
			}
			if err := writeMember(target, archive, header.FileInfo().Mode().Perm()&^0o022); err != nil {
				return files, err
			}
			files++
		default:
			return files, fmt.Errorf("the member %q is neither a directory nor a regular file", header.Name)
		}
	}
}

// mkdirShared makes target and every directory between it and dir, each 0755 whatever the umask:
// a role's shell may run umask 077, under which MkdirAll's 0755 lands 0700 and no one else could
// reach the check's files below it. dir itself is the caller's, and its mode is left to it. made
// is every directory it has made so far, target among them once it returns, each with every
// directory between it and dir: it makes none of them again and sets no mode of them again, since
// each member of a body would otherwise set the mode of each of its directories once more, and a
// syscall is slow under gVisor, which a pod runs under.
func mkdirShared(dir, target string, made map[string]bool) error {
	if made[target] {
		return nil
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	for at := target; at != dir && !made[at] && strings.HasPrefix(at, dir+string(filepath.Separator)); at = filepath.Dir(at) {
		if err := os.Chmod(at, 0o755); err != nil {
			return err
		}
		made[at] = true
	}
	made[target] = true
	return nil
}

// writeMember writes content to path as a new file of mode: O_EXCL, so a file already at path,
// whatever it is — a fifo a plain open would wait on, a symlink a plain open would write through —
// fails the write at once rather than being opened. unpackServed passes the member's own mode in
// the served body less group and other write: GitHub's tarballs carry git archive's 0664 and 0775
// (measured 2026-11-17 on codeload's tarball of sjawhar/knives v4.0.1), and no one but its owner
// may write a check every push runs. The mode is set with a chmod once the file is written rather
// than left to the open, whose mode the umask narrows: a role's shell may run umask 077, under
// which the check's python file would land 0600 and its launcher 0700, and the shared check must
// be readable by others.
func writeMember(path string, content io.Reader, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// verifyServed runs the freshly unpacked check once, before anything may use it, in the one shape
// that costs nothing and reads nothing of anyone's: `public-leak-check.sh --help`. start starts
// that run (ServedInstall.Start).
//
// It runs with a clean environment: PATH from this process's own, and HOME and TMPDIR set to the
// install's parent, which is also its working directory, and nothing else. The run is not in a
// role's own namespaces but in the launcher's, PID 1 of the roles' container, so beyond its
// environment it can read what that process can: /var/run/legion/roles/*, every role's launcher
// token, gh directory and private directory. That is accepted because whoever can move a tag of
// the check already runs code in every role on every write that role makes (plan D10).
//
// What is read is what it printed, never its status. Measured at leak-check-v15: `--help` prints
// the launcher's own usage header — the line naming the shapes this package calls, `check-public-text`
// and `gh-guard` — and exits 1; a copy whose python file did not land beside the launcher prints
// `LEAK CHECK ERROR: … is missing, so the check could not start` and exits 2, as does one whose
// /usr/bin/python3 will not start. So an install that cannot name its own shapes is a failed
// install, and a check that answers nothing at all is the same failure: this pod would otherwise
// run with a PUBLIC_LEAK_CHECK that refuses every write it is asked about.
func verifyServed(ctx context.Context, start procgroup.Starter, check, parent string) error {
	cmd := exec.CommandContext(ctx, check, "--help")
	procgroup.Configure(cmd)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + parent, "TMPDIR=" + parent}
	cmd.Dir = parent
	var printed strings.Builder
	cmd.Stdout, cmd.Stderr = &printed, &printed
	runErr := procgroup.Err(procgroup.Run(cmd, start))
	// A context that ended killed the run itself, so its end, not the kill, is the cause.
	if err := cmp.Or(ctx.Err(), procgroup.Said(runErr, printed.String())); err != nil {
		return fmt.Errorf("the served check could not be run: %w", err)
	}
	answer := printed.String()
	for _, shape := range []string{"check-public-text", "gh-guard"} {
		if !strings.Contains(answer, shape) {
			return fmt.Errorf("the served check did not answer: its --help names no %s shape: %s", shape, firstLine(answer))
		}
	}
	return nil
}

// firstLine is the first line printed holds that is not blank, which is what a refusal quotes of a
// check's answer.
func firstLine(printed string) string {
	for line := range strings.Lines(printed) {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return "it printed nothing"
}

// Installed is the path of the check the pod's launcher installed for the pin in env, which a
// role's shim reads before it starts anything that writes (shim.spawnOnce). dir is the install's
// directory; empty is InstallDir.
//
// With no pin in env it reads nothing, answers "" and succeeds. That is the tmux runtime: a
// pane's shim is started by the same command but from a machine's own environment, which carries
// no LEGION_LEAK_CHECK_TAG, and an operator's machine carries the check through the check's own
// installer rather than through a copy Legion puts into a developer's home. Where such a machine
// carries none under a deployment that names a check (CheckerVar), every entry point of this
// package refuses with a setup failure naming the missing check, which is the right answer on a
// machine that writes to public repositories with no check.
//
// With a pin but no CheckerVar in env it reads nothing either, answers "" and succeeds: the
// worker image carries its pin whatever the deployment runs, and a deployment that names no
// shared leak check (Setting.Configured) has the daemon push none to the pod's launcher and tells
// its roles no CheckerVar, so the role starts without one.
//
// With a pin and CheckerVar it answers a *SetupFailure, naming the pin and what the install holds,
// where the check is absent, where its tag file cannot be read (naming why), where the install
// records no tag, where it records another tag, and where the install's manifest digest
// (manifestSHA256: the launcher script, the program, the must-pass lines and the README) is not
// the deployment's or cannot be read: a role whose check is not the pin's, or not the one the
// deployment named by its manifest digest (Setting.ManifestSHA256), runs no agent.
func Installed(env []string, dir string) (string, error) {
	pin := value(env, TagVariable)
	if pin == "" || !configured(env) {
		return "", nil
	}
	if dir == "" {
		dir = InstallDir
	}
	check := filepath.Join(dir, checkName)
	tag, there, refused := installedTag(dir)
	switch {
	case refused != nil:
		return "", &SetupFailure{What: installWhat,
			Why: fmt.Sprintf("%s=%s, and the install at %s is no install of it: its tag file is refused (%v)", TagVariable, pin, dir, refused)}
	case !there:
		return "", &SetupFailure{What: installWhat,
			Why: fmt.Sprintf("%s=%s, and the pod's launcher installed no check at %s", TagVariable, pin, check)}
	case tag == "":
		return "", &SetupFailure{What: installWhat,
			Why: fmt.Sprintf("%s=%s, and the install at %s records no tag (%s)", TagVariable, pin, dir, tagFile)}
	case tag != pin:
		return "", &SetupFailure{What: installWhat,
			Why: fmt.Sprintf("%s=%s, and the install at %s is of %s", TagVariable, pin, dir, tag)}
	}
	checker := value(env, CheckerVar)
	held, err := manifestSHA256(check)
	switch {
	case err != nil:
		return "", &SetupFailure{What: installWhat,
			Why: fmt.Sprintf("%s=%s, and the manifest of the install at %s cannot be read to compare with %s=%s: %v", TagVariable, pin, dir, CheckerVar, checker, err)}
	case !strings.EqualFold(held, checker):
		return "", &SetupFailure{What: installWhat,
			Why: fmt.Sprintf("%s=%s, and the install at %s has the manifest digest %s, not the deployment's %s=%s", TagVariable, pin, dir, held, CheckerVar, checker)}
	}
	return check, nil
}

// tagLimit bounds how much of the tag file installedTag reads: leak-check-v<n> and its newline are
// 13 bytes and the digits of n, so the bound holds every tag whose n has at most 243 digits, and a
// file past it is taken as no install.
const tagLimit = 256

// installedTag is the tag of the install at dir and whether there is an install there at all: the
// check itself, with the tag file beside it where the install recorded one. A check with no tag
// file is an install of no tag a pin can match.
//
// The directory is under the issue's scratch, which every role of the pod writes, and the pod's
// launcher, its container's PID 1, reads the tag as it replaces an install (InstallServed). So the
// tag file is read as every file a role can write is read where something waits on the reader
// (rolefile.Read): a symlink there, to /dev/zero say, which a read that follows it and reads to the
// end would hold whole until PID 1 is killed, a fifo, whose plain open waits for a writer as long
// as none comes, anything else that is no regular file, and a file past tagLimit, are each
// refused, never followed or waited on. A refused read is taken as no install, and refused names
// why: an install replaces whatever is there, and a role's shim refuses to start its agent where
// there is no install of the pin, naming the refusal (Installed).
func installedTag(dir string) (tag string, there bool, refused error) {
	if _, err := os.Stat(filepath.Join(dir, checkName)); err != nil {
		return "", false, nil
	}
	recorded, err := rolefile.Read(filepath.Join(dir, tagFile), tagLimit)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", true, nil
	case err != nil:
		return "", false, err
	}
	return strings.TrimSpace(string(recorded)), true, nil
}
