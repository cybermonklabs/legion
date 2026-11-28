package shim

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// maxArgStrlen is Linux's MAX_ARG_STRLEN, the longest one argument of an exec may be, its NUL
// included: 32 pages of 4 KiB (execve(2)).
const maxArgStrlen = 32 * 4096

// ReadUnpushed hands unpushedRevset to jj as one argument, and that argument is what bounds
// shimwire.MaxReadUnpushedSessions; the frame that carries the same names is far inside
// shimwire.MaxFrameBytes. The longest revset a read the frame lets through can ask names that many
// sessions, each as long as claim.SessionIDRule lets it be and made of `.`, the one character of
// the rule the revset writes longer than itself (`\\.`, trailerRevset), and it is shorter than
// MAX_ARG_STRLEN, so it leaves a byte for its NUL.
func TestTheLongestUnpushedRevsetAReadCanAskFitsOneExecArgument(t *testing.T) {
	sessions := make([]string, shimwire.MaxReadUnpushedSessions)
	for i := range sessions {
		sessions[i] = strings.Repeat(".", 128)
	}
	read := shimwire.ReadUnpushed{ID: "read-1", Issue: "INFRA-945", Sessions: sessions, TimeoutMs: 1}
	if err := read.Validate(); err != nil {
		t.Fatalf("a read naming %d sessions of 128 `.`: Validate = %v", len(sessions), err)
	}
	if n := len(unpushedRevset(read)); n >= maxArgStrlen {
		t.Fatalf("the revset of a read naming %d sessions of 128 `.` is %d bytes, want under MAX_ARG_STRLEN, %d", len(sessions), n, maxArgStrlen)
	}
}
