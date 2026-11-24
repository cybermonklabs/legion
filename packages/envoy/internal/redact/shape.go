package redact

import (
	"regexp"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
)

// shape.go is the rule for a field another party wrote that is meant to have a fixed shape: a
// commit's SHA, a numeric id, a GitHub login. Such a field prints as it came only where it has its
// shape, and otherwise as a fixed phrase that names the field, so a value that is not the shape
// its field promises carries none of the words of whoever wrote it, a URL and its credential among
// them. Each function takes the field's name, a constant its call site writes ("head", "suite id",
// "installation login"), which the phrase names. A shape is checked where a value prints: stored
// data keeps its value, so no two values are stored under one phrase.

// shapedSHA is a commit's SHA: forty lowercase hex digits (SHA-1), or sixty-four (SHA-256).
var shapedSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// shapedID is a numeric id: one to twenty decimal digits, the most an unsigned 64-bit id takes.
var shapedID = regexp.MustCompile(`^[0-9]{1,20}$`)

// ShapedSHA is s, a commit's SHA another party wrote, as a message may print it: as it came where
// it is forty or sixty-four lowercase hex digits, and otherwise "(the <field> is not a SHA)".
func ShapedSHA(field, s string) string {
	if shapedSHA.MatchString(s) {
		return s
	}
	return "(the " + field + " is not a SHA)"
}

// ShapedID is s, a numeric id another party wrote, as a message may print it: as it came where it
// is one to twenty decimal digits, and otherwise "(the <field> is not an id)".
func ShapedID(field, s string) string {
	if shapedID.MatchString(s) {
		return s
	}
	return "(the " + field + " is not an id)"
}

// ShapedLogin is s, a GitHub login another party wrote, as a message may print it: as it came
// where it is what a GitHub login can be (githubname.ValidLogin, a managed user's login among
// them), and otherwise "(the <field> is not a login)".
func ShapedLogin(field, s string) string {
	if githubname.ValidLogin(s) {
		return s
	}
	return "(the " + field + " is not a login)"
}
