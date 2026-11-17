package brokerclient

import (
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

// Failure is the class of a failure that a person on this machine may be told how to cure. A
// caller names the class; Hint alone turns it into words, so no message can tell a person to run
// a login that would not cure its failure.
type Failure int

const (
	// FailureUnclassified is every failure no class below names. It gets no hint: a login cures
	// nothing a caller cannot say it cures.
	FailureUnclassified Failure = iota
	// FailureSignInRefused is a person's sign-in on this machine that no retry can renew: the
	// issuer ended it (ErrSignInEnded), the broker refused it for good (RefusedError.Permanent),
	// or this machine can no longer renew it (no sign-in client, or another client). A new sign-in
	// cures it.
	FailureSignInRefused
	// FailureNoSignIn is a machine with no usable sign-in of the person: nobody signed in, the
	// person named is not signed in here, or the record cannot be read. A sign-in cures it.
	FailureNoSignIn
	// FailureHeld is a person whom Dispatch holds (oauthwire.MembershipHeld, MEMBERSHIP_HELD). Their
	// next Dispatch sign-in, or the next refresh that confirms them, ends it, and the broker's own
	// words say so; a login on this machine would make a second box key and change nothing.
	FailureHeld
	// FailureBrokerUnavailable is a broker that did not answer, or answered 5xx or a refusal a
	// later trade may cure. Nothing on this machine cures it; the next trade may.
	FailureBrokerUnavailable
	// FailureIdentityLapsed is a helper in identity mode (AGENT_SECRETS_MACHINE_IDENTITY) that
	// holds no launcher credential: its trade loop trades this machine's identity for one on its
	// own, so nobody is told to log in.
	FailureIdentityLapsed
	// FailureNoMachineLogin is a helper in machine-login mode that holds no live launcher
	// credential, or one the broker refused: a machine login that a human approves cures it.
	FailureNoMachineLogin
)

// MachineLoginCommand is the command FailureNoMachineLogin names.
const MachineLoginCommand = "agent-secrets machine login"

// Hint is the one place that says what cures a failure of class f: "run: <command>" for a class a
// login cures, a sentence for one that cures itself, and "" for the rest. FailureUnclassified,
// FailureHeld and FailureBrokerUnavailable get "": a held person's refusal carries the broker's
// own words, and an outage needs nothing of the person.
func Hint(f Failure) string {
	switch f {
	case FailureSignInRefused, FailureNoSignIn:
		return "run: " + SignInCommand
	case FailureNoMachineLogin:
		return "run: " + MachineLoginCommand + ", and have a human approve it"
	case FailureIdentityLapsed:
		return "the helper's trade loop is trading this machine's identity for a new credential"
	default:
		return ""
	}
}

// WithHint is err followed by "; " and Hint(f), wrapping err, or err as it came when f has no
// hint.
func WithHint(err error, f Failure) error {
	hint := Hint(f)
	if err == nil || hint == "" {
		return err
	}
	return fmt.Errorf("%w; %s", err, hint)
}

// TradeFailure is the class of a failed trade of a person's sign-in: FailureSignInRefused for a
// refusal no retry cures, FailureHeld for a person Dispatch holds, and FailureBrokerUnavailable
// for every other refusal and for no answer at all. An ended sign-in (ErrSignInEnded) already
// carries its hint, so it is FailureUnclassified here and is not told twice.
func TradeFailure(err error) Failure {
	var refused *RefusedError
	switch {
	case errors.Is(err, ErrSignInEnded):
		return FailureUnclassified
	case errors.As(err, &refused) && refused.Code == oauthwire.MembershipHeld:
		return FailureHeld
	case errors.As(err, &refused) && refused.Permanent():
		return FailureSignInRefused
	default:
		return FailureBrokerUnavailable
	}
}
