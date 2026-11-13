package runtime

import "errors"

// LeakCheckRefusal is a role's start refused because the shared public leak check could not be
// put in place in the role's pod: the daemon could not read it, or the pod's launcher could not
// install what the daemon served. Cause is the one-line sentence that says which
// (leakguard.Cause), beginning with leakguard.RefusalPrefix, and is the error's whole text. The
// runtime refuses with it before it sends the start, so no process of the role was started.
type LeakCheckRefusal struct{ Cause string }

func (e LeakCheckRefusal) Error() string { return e.Cause }

// LeakCheckCause is the cause sentence of the LeakCheckRefusal in err's chain, or "" when err
// carries none: the start succeeded, or it failed for another reason.
func LeakCheckCause(err error) string {
	var refusal LeakCheckRefusal
	if errors.As(err, &refusal) {
		return refusal.Cause
	}
	return ""
}
