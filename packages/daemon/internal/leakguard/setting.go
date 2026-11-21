package leakguard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/rolefile"
)

// Setting is the deployment's shared public leak check as its operator names it: the daemon's
// `leak_check` block in legion.yaml, or RepositoryVar and ManifestVar in the daemon's environment
// (internal/config). Legion holds no name of it: the deployment supplies both values, and a
// turnkey install runs with neither.
//
// Repository is the repository the check is published from by tag, whose `leak-check` directory
// the daemon reads at a pod's pin and pushes to the pod's launcher (Source). ManifestSHA256 is
// that check's manifest digest (ManifestFiles, Manifest): one digest over every file the check
// runs from, which the runtime tells every role it starts under CheckerVar, and which a role's
// shim (Installed) and guard (Guard.sharedCheck) hold the check they find to.
//
// Both are set, or neither, which the loader holds the setting to. The zero Setting is a
// deployment that runs no shared leak check of its own: the daemon reads and pushes none, a role
// pod starts without it, and the guard refuses no write for its absence (Guard.sharedCheck).
type Setting struct {
	Repository     ghrepo.Repository
	ManifestSHA256 string
}

// Configured is whether the deployment names a shared leak check at all.
func (s Setting) Configured() bool { return !s.Repository.IsZero() }

// RepositoryVar is the daemon's environment fallback for Setting.Repository, which the file's
// `leak_check.repository` overrides.
const RepositoryVar = "LEGION_LEAK_CHECK_REPOSITORY"

// ManifestVar is the daemon's environment fallback for Setting.ManifestSHA256, the check's
// manifest digest (Manifest), which the file's `leak_check.manifest_sha256` overrides. It is the
// daemon's own and told to no role: a role is told the digest under CheckerVar, a name the
// daemon's configuration never reads, so a configuration loaded in a role — `legion config check`
// in a pod reads the process environment — never meets one half of the daemon's setting without
// the other.
const ManifestVar = "LEGION_LEAK_CHECK_MANIFEST_SHA256"

// CheckerVar is the name the runtime tells every role it starts the deployment's manifest digest
// (Setting.ManifestSHA256) under (runtime/sandbox's mainEnvironment, runtime/tmux's panePairs). In
// a role's environment it names the one check the role may write under: a role's shim reads the
// pod's install only under it and starts no agent unless the install's manifest digest is it
// (Installed), and the guard refuses a write for want of a check, or under a check whose manifest
// digest is anything else, only under it (Guard.sharedCheck). It does not decide `legion gh`'s
// checked-only reads, which only the resolved check's program does (GhRequest.ProvenCheck). It is
// exported because each of those reads it, and a second copy of the string in any of them is one
// an edit here would silently part from.
const CheckerVar = "LEGION_LEAK_CHECKER_MANIFEST_SHA256"

// configured is whether env names the deployment's manifest digest (CheckerVar): the role runs
// under a deployment that runs a shared leak check.
func configured(env []string) bool { return value(env, CheckerVar) != "" }

// ManifestFiles are the files the check's manifest digest covers, in the order it hashes them: the
// set the check's own install procedure installs into a home and records a sha256 of each of
// (`home_files` in its INSTALL.md). They are every file the guard runs or the check reads when it
// runs — the launcher script, which execs the program; the program; and the must-pass lines its
// self-test reads — and its README, which the procedure installs beside them.
var ManifestFiles = []string{checkName, checkProgram, "README.md", "must-pass-lines.txt"}

// Manifest is the manifest digest of the files read answers for ManifestFiles: for each file, in
// that order, the line `sha256sum` prints of it — its sha256 in lowercase hex, two spaces, its
// name, a newline — and the sha256, in lowercase hex, of those lines together. It is what
// `(cd <dir> && sha256sum public-leak-check.sh public_leak_check.py README.md must-pass-lines.txt)
// | sha256sum` prints of a directory holding the check, so an operator computes the setting's value
// with that one line. A file read cannot answer is an error naming it.
func Manifest(read func(name string) ([]byte, error)) (string, error) {
	return manifestFromSHA256(func(name string) ([sha256.Size]byte, error) {
		body, err := read(name)
		if err != nil {
			return [sha256.Size]byte{}, err
		}
		return sha256.Sum256(body), nil
	})
}

// manifestFromSHA256 writes the sha256sum line of each named digest in ManifestFiles' order and
// hashes those lines together. It keeps Manifest and the streamed digest of a served tarball on
// the same formula without retaining a second copy of its files.
func manifestFromSHA256(read func(name string) ([sha256.Size]byte, error)) (string, error) {
	var lines strings.Builder
	for _, name := range ManifestFiles {
		sum, err := read(name)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		lines.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	digest := sha256.Sum256([]byte(lines.String()))
	return hex.EncodeToString(digest[:]), nil
}

// manifestFileLimit bounds each read of a file the manifest covers: the program, the largest, is
// about 300 KB, and a file past the bound is refused rather than read whole.
const manifestFileLimit = 8 << 20

// runningCheckDir is the directory of the check that runs at check: check with its symbolic links
// followed, whose name must be the launcher's (checkName), since the manifest covers the launcher
// by that name and the launcher finds its program beside itself (readlink -f). A check named as a
// file of another name — a PUBLIC_LEAK_CHECK or pointer naming `other.sh`, or a
// public-leak-check.sh linked to `pass.sh` beside copies of the real files — names a file no
// manifest holds, and is refused.
func runningCheckDir(check string) (string, error) {
	followed, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", err
	}
	if filepath.Base(followed) != checkName {
		return "", fmt.Errorf("%s runs %s, which is not the %s its manifest covers", check, followed, checkName)
	}
	return filepath.Dir(followed), nil
}

// readManifestFile reads the file name of the check in dir as a file a role can write is read
// (rolefile.Read): no symbolic link followed, no fifo waited on, and bounded by
// manifestFileLimit. A link there would hash a file outside the directory, and a fifo or a link to
// /dev/zero would hold every role start and every guarded write of the pod.
func readManifestFile(dir, name string) ([]byte, error) {
	return rolefile.Read(filepath.Join(dir, name), manifestFileLimit)
}

// heldCheck is a check as one read of its four files took them (readHeldCheck): each file's bytes
// by its name, and their manifest digest. A guard under a deployment's digest runs these bytes and
// no others (Guard.sharedCheck, privateCopy), and `legion gh`'s checked-only read hashes its
// program from them (provenCheck), so nothing written to the install after the read reaches the
// write it was read for.
type heldCheck struct {
	files  map[string][]byte
	digest string
}

// readHeldCheck reads the four files of the check that runs at check, in its directory
// (runningCheckDir), each as readManifestFile reads it, and answers them with the manifest digest
// of the very bytes it holds: the buffers Manifest hashed are the ones kept.
func readHeldCheck(check string) (heldCheck, error) {
	dir, err := runningCheckDir(check)
	if err != nil {
		return heldCheck{}, err
	}
	held := heldCheck{files: make(map[string][]byte, len(ManifestFiles))}
	held.digest, err = Manifest(func(name string) ([]byte, error) {
		body, err := readManifestFile(dir, name)
		held.files[name] = body
		return body, err
	})
	if err != nil {
		return heldCheck{}, err
	}
	return held, nil
}

// manifestSHA256 is the manifest digest (Manifest) of the check that runs at check: of the four
// files in its directory (runningCheckDir), each read as readManifestFile reads it.
func manifestSHA256(check string) (string, error) {
	held, err := readHeldCheck(check)
	return held.digest, err
}
