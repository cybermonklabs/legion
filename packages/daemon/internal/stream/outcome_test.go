package stream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// ompBusySentence is how Oh My Pi words its refusal of a prompt that reaches it in a turn already
// running. The stream never reads it.
const ompBusySentence = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."

// A refused adoption of the working copy is the shim's own answer about a working copy whatever its
// words say, Oh My Pi's busy sentence among them: it is runtime.ErrAdoptRefused, which the
// supervisor names runtime.OutcomeAdoptionRefused and charges (supervise's refusedAdoption), and
// never a busy answer. A refused token read is no adoption.
func TestARefusedAdoptionIsTheAdoptionsRefusalWhateverItsWords(t *testing.T) {
	h := startListener(t, harnessOptions{})
	p := h.connect(testToken)
	conn := h.conn()
	identity := runtime.GitIdentity{Name: "legion-bot", Email: "bot@legion.dev"}
	result := async(func() error { return conn.AdoptWorkingCopy(context.Background(), identity, 3*time.Second) })
	request := p.expect(shimwire.TypeAdoptWorkingCopy).(shimwire.AdoptWorkingCopy)
	p.send(shimwire.AdoptWorkingCopyResult{ID: request.ID, OK: false, Error: ompBusySentence})
	err := awaitResult(t, result)
	if !errors.Is(err, runtime.ErrAdoptRefused) || errors.Is(err, runtime.ErrTokenRefused) {
		t.Fatalf("AdoptWorkingCopy = %#v, want the adoption's refusal whatever its words, and no token refusal", err)
	}
	if reason := runtime.RefusalReason(err); reason != ompBusySentence {
		t.Fatalf("runtime.RefusalReason(%v) = %q, want the shim's words %q for the log", err, reason, ompBusySentence)
	}
}
