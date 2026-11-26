// How the 1Password store words a call that failed: the call, and words of this program's own for
// the class of its failure (ErrDenied, ErrRateLimited, a deadline, or any other), never the text
// 1Password, the SDK or op wrote, which can quote what the call sent. The classes are read where
// each client sees its failure: sdk.go reads the SDK's errors, and op.go op's exit and its
// standard error, so this file imports no SDK package and builds on every system agent-secrets
// does.

package onepassword

import (
	"context"
	"errors"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// The permissions a reader of the vault needs, as 1Password names them for a service account
// (`op service-account create --help`: read_items) and for a person (the vault's permissions in
// the 1Password app): an item's fields are read with its value.
const (
	tagReadPermission   = "read access to the vault (read_items for a service account, View Items for a person)"
	valueReadPermission = "read access to the vault's values (read_items for a service account, View and Copy Passwords for a person)"
)

// failure is err, the failure of the 1Password call named call, as a message prints it
// (secretstore.Failure): the call, and the words of its class. errors.Is still finds ErrDenied,
// ErrRateLimited, secretstore.ErrNotFound and the context's errors through it. A nil err is nil.
func failure(call string, err error) error {
	if err == nil {
		return nil
	}
	var already secretstore.Failure
	if errors.As(err, &already) {
		return err
	}
	return secretstore.Failure{Call: call, Words: failureWords(err), Err: err}
}

// failureWords are the words of err's class.
func failureWords(err error) string {
	switch {
	case errors.Is(err, ErrDenied):
		return "1Password refused this sign-in the call; the vault's permissions decide what it may do"
	case errors.Is(err, ErrRateLimited):
		return "1Password refused the call at its request limit; try again once the limit has room"
	case errors.Is(err, secretstore.ErrNotFound):
		return "1Password holds no such item"
	case errors.Is(err, context.DeadlineExceeded):
		return "1Password did not answer within " + secretstore.CallDeadlineText()
	case errors.Is(err, context.Canceled):
		return "the call was stopped before 1Password answered"
	}
	var op *opError
	if errors.As(err, &op) {
		return op.Error() + "; run the same op command yourself to read op's message"
	}
	return "the call failed, and what 1Password answered is not printed"
}

// definiteRefusal reports whether err is 1Password answering before it acted: a denial or a
// refusal at its request limit, after which nothing was written.
func definiteRefusal(err error) bool {
	return errors.Is(err, ErrDenied) || errors.Is(err, ErrRateLimited)
}

// AccessDenied reports whether err is 1Password refusing this sign-in the call.
func (w *Writer) AccessDenied(err error) bool { return errors.Is(err, ErrDenied) }

// DefiniteRefusal reports whether 1Password answered err before it acted (definiteRefusal).
func (w *Writer) DefiniteRefusal(err error) bool { return definiteRefusal(err) }

// MissingPermission is none: 1Password's refusal names no permission this program could print.
func (w *Writer) MissingPermission(error) string { return "" }

func (w *Writer) TagReadPermission() string   { return tagReadPermission }
func (w *Writer) ValueReadPermission() string { return valueReadPermission }
