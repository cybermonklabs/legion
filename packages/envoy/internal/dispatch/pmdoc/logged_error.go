package pmdoc

import (
	"errors"
	"log/slog"
)

// refusalClasses are the particular refusals RefusalClass names a refusal of a document by, the
// most particular first. Each is a sentinel whose message is this package's own words, never a
// document's.
var refusalClasses = [...]error{
	ErrTablePadding,
	ErrBlockHTML,
	ErrMarkNotAllowed,
	ErrUnknownElement,
	ErrBlockPointer,
	ErrMisreadRendering,
	ErrRenderingRefused,
}

// A Refusal is a class of refusal a caller counts among refusals of a document beside this
// package's own (RefusalClass), and the words a log line names it by: the refusals that are a
// sentinel of the caller's (SentinelRefusal), or the refusals whose tree holds an error of a type
// of the caller's (TypeRefusal). A refusal of such a type carries its text in its fields - an
// ask's question, the markdown of a block - so it is no one value errors.Is can match, and only
// errors.As finds it. Its fields are unexported, so only those two make a Refusal that names
// anything (the zero Refusal names nothing), and each answers words fixed when it is made, never
// those of the error it names.
type Refusal struct {
	class string
	names func(error) bool
}

// SentinelRefusal is the class of the refusals that are sentinel (errors.Is), named by
// sentinel's own message: a caller's wrapper of a refusal, whose message is its own words.
func SentinelRefusal(sentinel error) Refusal {
	return Refusal{class: sentinel.Error(), names: func(err error) bool { return errors.Is(err, sentinel) }}
}

// TypeRefusal is the class of the refusals whose tree holds an error of type E (errors.As),
// named by class, words of the caller's own, since a message of E quotes what it refuses.
func TypeRefusal[E error](class string) Refusal {
	return Refusal{class: class, names: func(err error) bool {
		_, held := errors.AsType[E](err)
		return held
	}}
}

// LoggedError is err as a log line carries it. A refusal of a document - this package's (ErrSchema,
// ErrUnknownElement, ErrBlockPointer) or one a caller counts among refusals in refusals: a
// sentinel it wraps a refusal in, or an error type of its own that quotes a document or an ask -
// is logged as its class alone (RefusalClass). The refusal's own message is left out, and so is
// whatever a caller wrapped it in, because it quotes the document - a block's text, an attribute's
// value, an ask's question - and a document can hold a person's address and other private text,
// while a log line is read by whoever reads the server's logs. Any other error is logged as it is.
func LoggedError(err error, refusals ...Refusal) slog.Value {
	if class := RefusalClass(err, refusals...); class != "" {
		return slog.StringValue(class)
	}
	return slog.AnyValue(err)
}

// RefusalClass is the class a refusal of a document is logged by: the message of the first of
// this package's particular refusals err is (refusalClasses), then the class of the first of
// refusals that names err, in order, then ErrSchema's message, and "" where err refuses no
// document. It answers words fixed before err is seen and never err's, so a line that only ever
// logs a refusal logs it by this, and nothing of the refusal's message - the document's text it
// quotes - can reach that line whatever err holds.
func RefusalClass(err error, refusals ...Refusal) string {
	for _, class := range refusalClasses {
		if errors.Is(err, class) {
			return class.Error()
		}
	}
	for _, refusal := range refusals {
		if refusal.names != nil && refusal.names(err) {
			return refusal.class
		}
	}
	if errors.Is(err, ErrSchema) {
		return ErrSchema.Error()
	}
	return ""
}
