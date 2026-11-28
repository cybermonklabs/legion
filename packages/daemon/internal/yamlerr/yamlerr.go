// Package yamlerr is what a refusal of a YAML file the daemon cannot decode says of yaml.v3's
// error, in fixed words: that it does not parse, or holds a value of another kind than the file
// takes, at the line the error names, never the error's own text. That text quotes the document: a
// TypeError quotes the scalar it could not decode (whole up to ten characters, its first seven
// past that), and a reference to an anchor the file never set names the anchor, which is any value
// written after a "*". A credential in the file is such a scalar, or such a value, so a file that
// can hold one is refused through here.
package yamlerr

import (
	"errors"
	"regexp"

	"gopkg.in/yaml.v3"
)

// lineNumber is yaml.v3's own position in its error, `line <n>`, which comes before any quoted
// text in every error it returns: at the start of a parse error (`yaml: line 3: …`) and at the
// start of each of a TypeError's lines (`line 1: cannot unmarshal …`).
var lineNumber = regexp.MustCompile(`\bline ([0-9]+)\b`)

// Reason is what a refusal says of err, a yaml.v3 decode error: "the YAML does not parse", or,
// for a TypeError, "the YAML holds a value of another kind than the file takes", then " at line
// <n>" for the first line err names, and nothing more where it names none, as an unknown anchor's
// error does not.
func Reason(err error) string {
	reason := "the YAML does not parse"
	var typed *yaml.TypeError
	if errors.As(err, &typed) {
		reason = "the YAML holds a value of another kind than the file takes"
	}
	if match := lineNumber.FindStringSubmatch(err.Error()); match != nil {
		return reason + " at line " + match[1]
	}
	return reason
}
