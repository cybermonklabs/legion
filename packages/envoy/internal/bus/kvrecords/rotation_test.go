package kvrecords_test

//ci:reads packages/envoy/infra/README.md TestTheRotationReadFindsTheCopiesTheOldKeySigned reads it through ../../../infra/README.md

import (
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus/kvrecords"
	"github.com/sjawhar/envoy/internal/bus/signedrecord"
)

// copiesRead is infra/README.md's read of the records-only copies a key signed ("Rotating the hold
// signing key", step 3), with <old> the key's id and <copies> the directory the operator keeps the
// copies under: it counts the copies first and, when the directory holds none, says so on stderr,
// naming the directory, and exits 2; otherwise it lists each copy whose manifest.sig opens with the
// key's id and a dot, or holds no dot (written before signatures named their key), and exits 0
// when it lists one and 1 when it lists none. It runs in a subshell, so its exit does not close an
// interactive shell it is pasted into.
const copiesRead = `( count=$(find <copies> -name manifest.sig | wc -l)
if [ "$count" -eq 0 ]; then echo "no manifest.sig under <copies>: not the records-only copies' directory" >&2; exit 2; fi
grep -rlE --include=manifest.sig '^(<old>\.|[^.]+$)' <copies> )`

// The hold's records-only copies are signed with the operator's hold key, so a key rotation reads
// them too: infra/README.md's read, run as written over a directory of copies, lists the copy the
// old key signed and one written before signatures named their key, and not the new key's copy,
// and exits 0; run over a directory whose only copy the new key signed, it lists nothing and exits
// 1, the clean answer; run over a directory that holds no copy, it says so, naming the directory,
// and exits 2, so neither a person nor a script that reads stdout or the exit status takes a wrong
// directory for a clean one; and the old key's copy stops reading once that key leaves the list,
// naming its id. Fails while the README's step 3 reads only the five tables, as at e481b26500, or
// reads the copies with no count, as at 886868daac (a wrong directory prints nothing), or exits 0
// over an empty directory, as at a0a2c7622f, or exits 1 over an empty directory, the clean answer's
// status, as at 06d4ca2df1, or with a read that misses copies with no key id.
func TestTheRotationReadFindsTheCopiesTheOldKeySigned(t *testing.T) {
	readme, err := os.ReadFile("../../../infra/README.md")
	require.NoError(t, err)
	words := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	require.True(t, strings.Contains(words(string(readme)), words(copiesRead)), "infra/README.md's rotation step 3 does not hold the copies' read word for word: %s", copiesRead)

	r := newRig(t)
	r.fillBucket(t)
	copies := t.TempDir()
	old, oldKey := newKey(t)
	write := func(base, name string, key ed25519.PrivateKey) (string, kvrecords.Manifest) {
		t.Helper()
		dir := filepath.Join(base, name)
		manifest, err := kvrecords.Write(t.Context(), r.operator, bucket, dir, key)
		require.NoError(t, err)
		return dir, manifest
	}
	oldCopy, oldManifest := write(copies, "old", oldKey)
	write(copies, "new", r.key)
	legacy, _ := write(copies, "legacy", oldKey)
	sigPath := filepath.Join(legacy, kvrecords.SignatureFile)
	sig, err := os.ReadFile(sigPath)
	require.NoError(t, err)
	_, bare, named := strings.Cut(string(sig), ".")
	require.True(t, named, "the copy's signature %q names no key", sig)
	require.NoError(t, os.WriteFile(sigPath, []byte(bare), 0o600))

	read := func(dir string) *exec.Cmd {
		return exec.Command("sh", "-c", strings.NewReplacer("<old>", signedrecord.KeyID(old), "<copies>", dir).Replace(copiesRead))
	}
	out, err := read(copies).Output()
	require.NoError(t, err, "the read over %s", copies)
	var found []string
	for _, line := range strings.Fields(string(out)) {
		rel, err := filepath.Rel(copies, line)
		require.NoError(t, err)
		found = append(found, rel)
	}
	slices.Sort(found)
	require.Equal(t, []string{filepath.Join("legacy", kvrecords.SignatureFile), filepath.Join("old", kvrecords.SignatureFile)}, found,
		"the read by the old key's id lists the old key's copy and the copy with no key id, and not the new key's")

	exitCode := func(err error) int {
		t.Helper()
		if err == nil {
			return 0
		}
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit, "the read did not run")
		return exit.ExitCode()
	}

	clean := t.TempDir()
	write(clean, "new", r.key)
	said, err := read(clean).CombinedOutput()
	require.Empty(t, string(said), "the read over a directory whose only copy the new key signed prints %q; want nothing", said)
	require.Equal(t, 1, exitCode(err), "the read over a directory whose only copy the new key signed (the clean answer) exited with %v; want exit status 1", err)

	empty := t.TempDir()
	said, err = read(empty).CombinedOutput()
	require.Contains(t, string(said), "no manifest.sig under "+empty+": not the records-only copies' directory",
		"the read over a directory with no copy prints %q; want it to say the directory holds none", said)
	require.Equal(t, 2, exitCode(err), "the read over a directory with no copy exited with %v; want exit status 2, which the clean answer's 1 cannot be taken for", err)

	_, _, err = kvrecords.Read(oldCopy, bucket, r.keys, oldManifest.ManifestSHA256)
	var unlisted *signedrecord.UnlistedKeyError
	require.ErrorAs(t, err, &unlisted, "the old key's copy, read with the new key alone")
	require.Equal(t, signedrecord.KeyID(old), unlisted.KeyID)
}
