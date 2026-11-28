package machine

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/store"
)

// timeApprovalsEnv, set to anything, has this package's tests print how long each approval they
// decide took (applyDecision), one line each: "APPROVAL-DECIDED-IN <nanoseconds> ns". The greatest
// of them under load is the broker's half of the measured in-flight time ApprovalInFlightWindow's
// comment gives as context beside its bound.
const timeApprovalsEnv = "ENVOY_TIME_APPROVALS"

// timeApprovals is whether timeApprovalsEnv was set when the tests started.
var timeApprovals = os.Getenv(timeApprovalsEnv) != ""

// applyDecision is s.ApplyDecision, called with ctx and the rest as given and answering its results
// unchanged: the one way this package's tests decide a machine login. With timeApprovals it prints
// how long an approval took from the call to its return (its commit, or the refusal it answers),
// lock waits included.
func applyDecision(ctx context.Context, s *Service, recordID string, approve bool, login, code string, account store.IssuerSubject, point *int64) (state, credentialID string, err error) {
	if !approve || !timeApprovals {
		return s.ApplyDecision(ctx, recordID, approve, login, code, account, point)
	}
	began := time.Now()
	state, credentialID, err = s.ApplyDecision(ctx, recordID, approve, login, code, account, point)
	fmt.Fprintf(os.Stderr, "APPROVAL-DECIDED-IN %d ns\n", time.Since(began).Nanoseconds())
	return state, credentialID, err
}
