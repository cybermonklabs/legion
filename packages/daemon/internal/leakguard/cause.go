package leakguard

import (
	"strings"
	"unicode/utf8"
)

// RefusalPrefix begins every cause sentence of a start refused because the pod's shared public
// leak check could not be put in place: the daemon's own causes (its read of the check's
// repository) and the pod's (its launcher's install of what the daemon served). A reader of a
// claim finds the cause by this prefix, and the daemon tells the launcher's own sentence from a
// bare error by it.
const RefusalPrefix = "the shared public leak check could not be installed: "

// maxCauseBytes is the most a cause sentence carries: it is one line of a claim, of the state and
// of `legion claims list`, so a long error underneath is cut rather than carried whole.
const maxCauseBytes = 400

// oneLine turns every line break of a cause sentence into a space.
var oneLine = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// Cause is the one-line cause sentence of a check that could not be installed: RefusalPrefix,
// then what happened, then err's text after a colon where err is not nil. A what that already
// begins with RefusalPrefix — the launcher's own sentence, which the daemon carries on — is not
// prefixed a second time. Newlines become spaces, so the sentence stays one line, and it is cut
// to at most maxCauseBytes at a rune boundary.
func Cause(what string, err error) string {
	sentence := what
	if !strings.HasPrefix(sentence, RefusalPrefix) {
		sentence = RefusalPrefix + sentence
	}
	if err != nil {
		sentence += ": " + err.Error()
	}
	sentence = oneLine.Replace(sentence)
	if len(sentence) <= maxCauseBytes {
		return sentence
	}
	cut := maxCauseBytes
	for cut > 0 && !utf8.RuneStart(sentence[cut]) {
		cut--
	}
	return sentence[:cut]
}
