package leakguard

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
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

// tagPattern is the published shape of a tag of the check's repository. internal/shimwire writes
// the same pattern for the frames that carry the check, so the wire imports nothing of this
// package; a test of this package holds the two equal.
const tagPattern = `^leak-check-v[1-9][0-9]*$`

var tagShape = regexp.MustCompile(tagPattern)

// ValidTag is whether tag is of the published shape leak-check-v<n>. The daemon refuses any other
// tag before it reads anything, since a tag becomes a path of a GitHub API endpoint.
func ValidTag(tag string) bool { return tagShape.MatchString(tag) }

// Where the shared check is published: a tag of a private repository, whose `leak-check` directory
// holds the launcher (checkName), the python file the launcher runs from beside itself, and the
// fixtures its own self-test reads. The whole directory is what the daemon packs and the launcher
// installs for that reason — a launcher without its neighbours is a check that cannot start.
const (
	checkOwner = "theorem-labs"
	checkRepo  = "agent-infra"
	// CheckSlug is the check's repository, which the daemon reads with a token of its own.
	CheckSlug      = checkOwner + "/" + checkRepo
	checkDirectory = "leak-check"
)

// installWhat is what a failed read of the install is reported as, in the words a
// *SetupFailure's What takes.
const installWhat = "the pod's install of the shared public leak check"

// installBound is how long the launcher's whole install has: the unpack, the one verification run
// and the renames together. The body is in memory by then, so this is wide enough for a slow node
// several times over and still ends the install rather than hanging the launcher's answer.
const installBound = 5 * time.Minute

// The read limits of PackTarball. The tarball of the check's repository was 16 MB at
// leak-check-v15 and its leak-check directory about one, so each is far above what the repository
// holds and still bounds what one read may take of the daemon's memory.
const (
	maxTarballBytes = 256 << 20
	maxPackedBytes  = 16 << 20
)

// errOverLimit is what a capped reader or writer answers once its limit is passed; the caller
// reads which one passed it from the cap itself.
var errOverLimit = errors.New("over the read limit")

// PackTarball reads a GitHub tarball of the check's repository (gzip, then tar), and answers its
// leak-check directory alone, packed into a new gzipped tarball whose member paths are the
// directory's own — no repository prefix and no leading `leak-check/` — with each file's bytes
// and mode as the tarball has them, and how many files it holds. That body is what the daemon
// serves to a pod's launcher (InstallServed).
//
// It refuses a member of that directory that is neither a directory nor a regular file, a member
// whose path does not stay inside it, a tarball with no such directory, a tarball over
// maxTarballBytes and a directory that packs over maxPackedBytes. Each error is a predicate of
// the tarball, written to follow `the tarball of <CheckSlug> at <tag> ` in the cause sentence the
// daemon builds of it (Cause).
func PackTarball(r io.Reader) (body []byte, files int, err error) {
	in := &cappedReader{r: r, left: maxTarballBytes}
	out := &cappedBuffer{left: maxPackedBytes}
	files, found, err := pack(in, out)
	switch {
	case in.over:
		return nil, 0, fmt.Errorf("is over the %d MiB this daemon reads of one", maxTarballBytes>>20)
	case out.over:
		return nil, 0, fmt.Errorf("holds a %s directory that packs over the %d MiB this daemon serves", checkDirectory, maxPackedBytes>>20)
	case err != nil:
		return nil, 0, err
	case !found:
		return nil, 0, fmt.Errorf("holds no %s directory", checkDirectory)
	}
	return out.buf.Bytes(), files, nil
}

// pack copies the leak-check directory of the tarball r carries into a gzipped tarball written to
// w, and answers how many files it wrote and whether the directory was there at all. GitHub's
// tarball of a repository holds every member under one `<owner>-<repo>-<sha>/` directory, so the
// members this reads are `<that>/leak-check/…` (memberPath).
func pack(r io.Reader, w io.Writer) (files int, found bool, err error) {
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
			err = packed.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: rel, Mode: mode, Size: header.Size, ModTime: header.ModTime})
			if err == nil {
				_, err = io.Copy(packed, archive)
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

// cappedReader reads r and answers errOverLimit, setting over, once more than left bytes came.
type cappedReader struct {
	r    io.Reader
	left int64
	over bool
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.over {
		return 0, errOverLimit
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	if int64(n) > c.left {
		c.over = true
		return 0, errOverLimit
	}
	c.left -= int64(n)
	return n, err
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

// ServedInstall is what InstallServed installs: the body the daemon served (PackTarball's), the
// tag it is of, and where it goes.
type ServedInstall struct {
	// Dir is where the check is installed; empty is InstallDir.
	Dir string
	// Tag is the tag the body is of, which the install records beside the check (tagFile).
	Tag string
	// Body is the packed check: a gzipped tarball of the leak-check directory's own paths.
	Body []byte
	// Log receives the install's one line where it replaced what Dir held. Nil logs nothing.
	Log func(format string, args ...any)
}

func (in ServedInstall) logf(format string, args ...any) {
	if in.Log != nil {
		in.Log(format, args...)
	}
}

// InstallServed installs the check the daemon served into in.Dir and answers the installed
// check's path. The pod's launcher runs it once for the pod, before any role starts; every role's
// shim then reads the install (Installed).
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
func InstallServed(ctx context.Context, in ServedInstall) (string, error) {
	if !ValidTag(in.Tag) {
		return "", fmt.Errorf("the pod's launcher was served the tag %q, which is not of the published shape leak-check-v<n>", in.Tag)
	}
	dir := in.Dir
	if dir == "" {
		dir = InstallDir
	}
	ctx, cancel := context.WithTimeout(ctx, installBound)
	defer cancel()
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("the install directory's parent could not be made: %w", err)
	}
	// Beside the final directory, so the rename below is a rename within one filesystem, and
	// hidden, so a half-unpacked directory is never mistaken for the install by a reader looking
	// at the scratch.
	staging, err := os.MkdirTemp(parent, ".leak-check-install-")
	if err != nil {
		return "", fmt.Errorf("the staging directory could not be made: %w", err)
	}
	defer os.RemoveAll(staging)
	files, err := unpackServed(in.Body, staging)
	if err == nil && files == 0 {
		err = errors.New("it holds no file")
	}
	if err != nil {
		return "", fmt.Errorf("the pod's launcher could not unpack the check the daemon served: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, tagFile), []byte(in.Tag+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("the installed tag could not be recorded: %w", err)
	}
	if err := verifyServed(ctx, filepath.Join(staging, checkName), parent); err != nil {
		return "", err
	}
	// 0700 is what MkdirTemp makes. The roles of a pod run as one user, so the mode decides
	// nothing between them, but the install is read by every role and read-only to all of them.
	if err := os.Chmod(staging, 0o755); err != nil {
		return "", fmt.Errorf("the staged install could not be made readable: %w", err)
	}
	check := filepath.Join(dir, checkName)
	if _, err := os.Lstat(dir); err == nil {
		// Something is in the way — an install of another tag, one of the same tag a restarted
		// launcher installs over, or anything else — and a rename onto a directory is refused: by
		// every filesystem for one that is not empty, and by gVisor, which a pod runs under, for
		// an empty one too. So it is moved aside, to a name beside the staging directory's and so
		// on its filesystem, and removed once the new one is in place rather than before it: the
		// path names a whole check at every moment but the one between the two renames, and a
		// second rename that fails puts the old one back.
		held := "what " + dir + " held"
		if tag, there := installedTag(dir); there && tag != "" {
			held = "the install of " + tag + " at " + dir
		}
		aside := staging + ".replaced"
		defer os.RemoveAll(aside)
		if err := os.Rename(dir, aside); err != nil {
			return "", fmt.Errorf("%s could not be moved aside: %w", held, err)
		}
		if err := os.Rename(staging, dir); err != nil {
			_ = os.Rename(aside, dir)
			return "", fmt.Errorf("the staged install could not be moved to %s: %w", dir, err)
		}
		in.logf("the install of %s replaced %s", in.Tag, held)
		return check, nil
	}
	if err := os.Rename(staging, dir); err != nil {
		return "", fmt.Errorf("the staged install could not be moved to %s: %w", dir, err)
	}
	return check, nil
}

// unpackServed writes the served body into dir and answers how many files it wrote. The body
// carries the leak-check directory's own paths, so every member is written at its own path under
// dir. A member whose path does not stay inside dir and one that is neither a directory nor a
// regular file fail the unpack rather than being skipped, and no file is written over another
// (writeMember's O_EXCL): a body holding either is not the body PackTarball makes.
func unpackServed(body []byte, dir string) (int, error) {
	zipped, err := gzip.NewReader(bytes.NewReader(body))
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
		if !filepath.IsLocal(header.Name) {
			return files, fmt.Errorf("the member %q is no path inside the check's directory", header.Name)
		}
		target := filepath.Join(dir, header.Name)
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

// verifyServed runs the freshly unpacked check once, before anything may use it, in the one shape
// that costs nothing and reads nothing of anyone's: `public-leak-check.sh --help`.
//
// It runs with a clean environment: PATH from this process's own, and HOME and TMPDIR set to the
// install's parent, which is also its working directory, and nothing else. The launcher that runs
// it is the pod's first process and holds every role's launcher token, so the check is handed
// none of what that process carries, and writes only under the install's parent.
//
// What is read is what it printed, never its status. Measured at leak-check-v15: `--help` prints
// the launcher's own usage header — the line naming the shapes this package calls, `check-public-text`
// and `gh-guard` — and exits 1; a copy whose python file did not land beside the launcher prints
// `LEAK CHECK ERROR: … is missing, so the check could not start` and exits 2, as does one whose
// /usr/bin/python3 will not start. So an install that cannot name its own shapes is a failed
// install, and a check that answers nothing at all is the same failure: this pod would otherwise
// run with a PUBLIC_LEAK_CHECK that refuses every write it is asked about.
func verifyServed(ctx context.Context, check, parent string) error {
	cmd := exec.CommandContext(ctx, check, "--help")
	procgroup.Configure(cmd)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + parent, "TMPDIR=" + parent}
	cmd.Dir = parent
	var printed strings.Builder
	cmd.Stdout, cmd.Stderr = &printed, &printed
	if err := procgroup.Err(cmd.Run()); err != nil {
		return fmt.Errorf("the served check could not be run: %w", err)
	}
	if err := ctx.Err(); err != nil {
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
// carries none, every entry point of this package refuses with a setup failure naming the
// missing check, which is the right answer on a machine that writes to public repositories with
// no check.
//
// With a pin it answers a *SetupFailure, naming the pin and what the install holds, where the
// check is absent, where the install records no tag, and where it records another tag: a role
// whose check is not the pin's runs no agent.
func Installed(env []string, dir string) (string, error) {
	pin := value(env, TagVariable)
	if pin == "" {
		return "", nil
	}
	if dir == "" {
		dir = InstallDir
	}
	check := filepath.Join(dir, checkName)
	tag, there := installedTag(dir)
	switch {
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
	return check, nil
}

// installedTag is the tag of the install at dir and whether there is an install there at all: the
// check itself, with the tag file beside it where the install recorded one. A check with no tag
// file is an install of no tag a pin can match.
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
