package runtime

import (
	"errors"
	"fmt"
	"testing"
)

// minted is a refusal carrying the member its minter gave it, as the stream's refusal does.
type minted struct{ outcome Outcome }

func (m minted) Error() string           { return "refused as " + string(m.outcome) }
func (m minted) RefusalOutcome() Outcome { return m.outcome }

// RefusalOutcome reads the member a refusal in the chain carries, wrapped or not, and an error
// that carries none — a transport failure, a refusal with words alone — is OutcomeUnrecognised.
func TestRefusalOutcomeReadsTheMintedMemberAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want Outcome
	}{
		{"a minted refusal", minted{OutcomeAgentBusy}, OutcomeAgentBusy},
		{"a minted refusal, wrapped", fmt.Errorf("send: %w", minted{OutcomeAdoptionRefused}), OutcomeAdoptionRefused},
		{"a refusal with words alone", fmt.Errorf("Agent is busy: %w", ErrPromptRefused), OutcomeUnrecognised},
		{"a transport failure", errors.New("connection closed"), OutcomeUnrecognised},
		{"no error", nil, OutcomeUnrecognised},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RefusalOutcome(tc.err); got != tc.want {
				t.Fatalf("RefusalOutcome(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
