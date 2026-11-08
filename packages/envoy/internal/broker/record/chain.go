package record

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ErrChainBroken is every reason ChainVerifier.Verify refuses: the record does not exist, its
// stored body does not reproduce its own id, its embedded request object does not verify, or it
// does not carry exactly one terminal decision that is an approval by its approver. A caller that
// only cares "does this still authenticate" collapses every case with errors.Is(err,
// ErrChainBroken); one that needs the reason keeps the wrapped detail.
var ErrChainBroken = errors.New("credential request chain does not verify")

// ErrNotApprover is a decision on a record by a login other than the approver the record names.
var ErrNotApprover = errors.New("only the record's approver may decide it")

// TerminalEvent is one terminal decision event a record carries — approved, denied, expired or
// cancelled — the login that decided it, "" for an event no human decided, and the audit actor
// the event was written under: HumanActor for a person's decision, JudgeActor for one the model
// judge made, which carries no login at all, and the broker itself for an event it ended on its
// own.
type TerminalEvent struct {
	Event string
	Login string
	Actor string
}

// terminalEventNames are the events that end a record's pending state, which a record carries at
// most once: credential_request_decision's partial unique index, whose predicate spells the same
// list, holds it to that.
var terminalEventNames = []string{"approved", "denied", "expired", "cancelled"}

// TerminalEventNames returns a copy of the terminal events, for a query to pass as
// `event = any($n)`; a copy, so no caller can change the list another caller reads.
func TerminalEventNames() []string {
	return slices.Clone(terminalEventNames)
}

// ChainVerifier re-derives a credential-request record's full issuance chain rather than
// trusting a downstream row (a launcher credential, a grant) that merely names the record's id:
// the stored body must reproduce the record's own content-addressed id, the request object it
// embeds must still be a well-formed, signed proof — checked as of the record's own creation
// time, not now, since a request object's own ~10-minute exp is long past by the time anything
// built from it is used again; re-verifying it proves provenance, not freshness — and the record
// must carry exactly one terminal decision, an approval by a login the record's rule admits: its
// approver, or, for a service's machine login, any login — or, for an approval the model judge
// made, one the airlock rules let the judge make at all (MayJudge).
//
// store.Store.ChainVerifier builds the one enroll.AuthenticateLauncher and requests.Machine's own
// chain check each use, by record kind, through these narrow func fields, so this package needs no
// store access of its own.
type ChainVerifier struct {
	// Kind is the kind of record FetchRecord serves (KindAgentSecret or KindLauncherCredential),
	// which, with the service its request object names, decides who may have approved it
	// (MayDecide).
	Kind string

	// Audience and Skew re-verify the embedded request object exactly as VerifyRequestObject
	// enforced them when the record was first created.
	Audience string
	Skew     time.Duration

	// FetchRecord resolves a record id to its stored canonical body and creation time. found is
	// false when no such record exists — including, per the caller's own query, one that exists
	// but is not of the kind the caller cares about.
	FetchRecord func(ctx context.Context, recordID string) (body string, createdAt time.Time, found bool, err error)

	// FetchDecisions resolves every terminal decision event the record carries, oldest first.
	// The broker writes at most one (credential_request_decision's partial unique index holds it
	// to that), so a second is a row something other than the broker wrote.
	FetchDecisions func(ctx context.Context, recordID string) ([]TerminalEvent, error)

	// JudgeSeconds is the airlock rules' bound on the lifetime the model judge may decide for
	// one secret (airlock.Rules.JudgeSeconds), which MayJudge holds a judge's approval to. It
	// is a func field for the same reason MayJudge takes one: this package must not import
	// airlock. A verifier that leaves it nil never admits a judge's approval, since MayJudge
	// refuses with no bound available — which is what enroll.NewChainVerifier does, a launcher
	// credential being no record the judge ever decides.
	JudgeSeconds func(secret string) (uint32, bool)
}

// Verify re-derives recordID's full issuance chain and returns its parsed Body once every link
// holds.
//
// Which arm admits the approval is read off the event's actor and never its login: the judge's
// own approval is written under JudgeActor with a null login on purpose, so no owner tag or
// login_hint can dress it up as a person's — MayDecide refuses a login shaped "judge:" for that
// same reason — and an event with any other actor is a person's, held to MayDecide as before.
func (c *ChainVerifier) Verify(ctx context.Context, recordID string) (Body, error) {
	canonical, createdAt, found, err := c.FetchRecord(ctx, recordID)
	if err != nil {
		return Body{}, err
	}
	if !found {
		return Body{}, fmt.Errorf("%w: no such record", ErrChainBroken)
	}
	body, err := VerifyBodyReproducesID(canonical, recordID)
	if err != nil {
		if errors.Is(err, ErrBodyIDMismatch) {
			return Body{}, fmt.Errorf("%w: stored body does not reproduce its own id", ErrChainBroken)
		}
		return Body{}, fmt.Errorf("%w: stored body does not parse: %s", ErrChainBroken, err)
	}
	obj, err := VerifyRequestObject(body.Request, c.Audience, c.Skew, createdAt)
	if err != nil {
		return Body{}, fmt.Errorf("%w: request object: %s", ErrChainBroken, err)
	}
	decisions, err := c.FetchDecisions(ctx, recordID)
	if err != nil {
		return Body{}, err
	}
	switch {
	case len(decisions) == 0:
		return Body{}, fmt.Errorf("%w: no approved event", ErrChainBroken)
	case len(decisions) > 1:
		return Body{}, fmt.Errorf("%w: %d terminal decision events, want exactly one", ErrChainBroken, len(decisions))
	case decisions[0].Event != "approved":
		return Body{}, fmt.Errorf("%w: no approved event (decided %s)", ErrChainBroken, decisions[0].Event)
	}
	if modelID, judged := JudgeLogin(decisions[0].Actor); judged {
		if err := MayJudge(body, obj, c.JudgeSeconds); err != nil {
			return Body{}, fmt.Errorf("%w: approved by the model judge %q, which the rules do not admit: %s", ErrChainBroken, modelID, err)
		}
	} else if !MayDecide(c.Kind, body.Approver, obj.Service(), decisions[0].Login) {
		return Body{}, fmt.Errorf("%w: approved by %q, whom the record's rule does not admit (approver %q)", ErrChainBroken, decisions[0].Login, body.Approver)
	}
	return body, nil
}
