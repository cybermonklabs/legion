package brokerclient

import "fmt"

// Failure is the class of a failure, or of a state, that a person on this machine may be told the
// cure or the course of. A caller names the class; Hint and Lapse alone turn it into words, so no
// message can tell a person to run a login that would not cure its failure, or promise a renewal
// that nothing makes.
type Failure int

const (
	// FailureUnclassified is every failure no class below names. It gets no hint: a login cures
	// nothing a caller cannot say it cures.
	FailureUnclassified Failure = iota
	// FailureSignInRefused is a person's sign-in on this machine that this machine knows no retry
	// can renew: the issuer ended it (ErrSignInEnded), or this machine can no longer renew it (no
	// sign-in client, or another client than the one it was made with). A new sign-in cures it.
	FailureSignInRefused
	// FailureNoSignIn is a machine with no usable sign-in of the person: nobody signed in, the
	// person named is not signed in here, or the record cannot be read. A sign-in cures it.
	FailureNoSignIn
	// FailureIdentityLapsed is a helper in identity mode (AGENT_SECRETS_MACHINE_IDENTITY) that
	// holds no launcher credential while its trade loop runs: the loop trades this machine's
	// identity for one on its own, so nobody is told to log in.
	FailureIdentityLapsed
	// FailureIdentityStopped is a helper in identity mode whose trade loop has stopped: the broker
	// refused this machine's identity in a way no retry cures (a machine no entry lists, say), the
	// helper's log names the refusal, and nothing trades again until the deployment is fixed and
	// the helper restarts. No login cures it either.
	FailureIdentityStopped
	// FailureNoMachineLogin is a helper in machine-login mode that holds no live launcher
	// credential, or one the broker refused: a machine login that a human approves cures it.
	FailureNoMachineLogin
)

// MachineLoginCommand is the command FailureNoMachineLogin names.
const MachineLoginCommand = "agent-secrets machine login"

// Hint is the one place in this module's clients that says which login cures a failure of class
// f: "run: <command>" for a class a login cures, a sentence for one that cures itself, and "" for
// the rest. FailureUnclassified gets "": a broker refusal carries the broker's own words (a
// sign-in it revoked names `agent-secrets login` there), and a person Dispatch holds, a machine no
// entry lists, or a broker that did not answer needs no login, so a failed trade adds nothing.
func Hint(f Failure) string {
	switch f {
	case FailureSignInRefused, FailureNoSignIn:
		return "run: " + SignInCommand
	case FailureNoMachineLogin:
		return "run: " + MachineLoginCommand + ", and have a human approve it"
	case FailureIdentityLapsed:
		return "the helper's trade loop is trading this machine's identity for a new credential"
	case FailureIdentityStopped:
		return "the helper stopped trading this machine's identity, and its log gives the reason; restart agent-secrets-helper once that is fixed"
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

// Lapse is the sentence about a launcher credential still live, which lapses at at, in left (a
// rounded duration as the caller prints it), under the helper whose class is f: what renews it,
// or, once the helper's trade loop has stopped, that nothing does, and the hint for that. Only
// FailureNoMachineLogin, FailureIdentityLapsed and FailureIdentityStopped describe a helper that
// holds a credential; any other class reads as machine-login mode, the mode a helper from before
// these classes reports.
func Lapse(f Failure, at, left string) string {
	switch f {
	case FailureIdentityLapsed:
		return fmt.Sprintf("the helper's trade loop renews the launcher credential; without a trade it lapses at %s (in %s)", at, left)
	case FailureIdentityStopped:
		return fmt.Sprintf("nothing renews the launcher credential, which lapses at %s (in %s), and no session enrols under this machine's credential after that; %s", at, left, Hint(f))
	default:
		return fmt.Sprintf("the machine login renews itself; without a refresh it lapses at %s (in %s)", at, left)
	}
}
