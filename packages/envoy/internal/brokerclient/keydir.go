// packages/envoy/internal/brokerclient/keydir.go
//
// The files a session's identity is kept in within an agent-secrets key directory
// (AGENT_SECRETS_KEY_DIR), and the one way a machine session kept there is dropped.
//
// cmd/agent-secrets writes and reads these files under these names (identity.go, where they
// were declared until `legion controller start` needed them too). That start drops the machine
// session agent-secrets kept in the controller's own key directory whenever a helper socket is
// set or present (packages/daemon's controllerKeys), so both name the files from here and a
// rename on one side cannot leave the other dropping files nobody writes.

package brokerclient

import (
	"errors"
	"os"
	"path/filepath"
)

const (
	// KeyFile and EnrollmentFile are a box's or pod's identity in its key dir: the key keygen
	// writes, and the enrollment id enroll (or the pod's launcher) writes beside it.
	KeyFile        = "key.pem"
	EnrollmentFile = "enrollment"
	// EnrollmentErrorFile is what whoever wrote the key records a refused enrollment in, beside
	// it: key.pem with no enrollment is the one state cmd/agent-secrets's selectSigner cannot
	// read on its own, since it takes the key dir's branch on the key alone and then finds no id.
	// Its content is the refusal, which selectSigner appends to what it answers, so a later call
	// says what the broker said rather than naming a missing file.
	EnrollmentErrorFile = "enrollment.error"
	// MachineSessionMarker says this key dir's key and enrollment are a session agent-secrets
	// registered for a machine that proves an identity of its own, which is what makes the
	// enrollment one agent-secrets may register again when the broker says it is not live, and
	// one DropMachineSession may drop. A launcher's key dir holds no such file, nor does one
	// `agent-secrets enroll --pod` wrote: that enrollment is the launcher's, or the pod's, to make
	// again.
	MachineSessionMarker = "machine-session"
)

// DropMachineSession removes the machine session agent-secrets registered and kept in dir — its
// KeyFile, its EnrollmentFile, a refused enrollment's EnrollmentErrorFile, and the
// MachineSessionMarker — and answers whether dir held one. A dir with no marker holds no such
// session, whatever else it holds (a launcher's key, or the one `agent-secrets enroll --pod`
// wrote, neither of which is agent-secrets's to drop), and is left as it is, answering false; so
// is a dir that does not exist.
//
// The key goes first and the marker last: a removal that fails part way leaves no key the signer
// takes once the key is gone, and leaves the marker, so the next drop still recognises the dir as
// a machine session and finishes it. A file already gone is no failure. An error is the removal
// that failed, naming its path.
func DropMachineSession(dir string) (bool, error) {
	marker := filepath.Join(dir, MachineSessionMarker)
	if _, err := os.Lstat(marker); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	for _, name := range []string{KeyFile, EnrollmentFile, EnrollmentErrorFile, MachineSessionMarker} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return true, nil
}
