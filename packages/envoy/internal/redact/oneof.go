package redact

// OneOf is the word of words the field is, compared as it came (Equal), and true; "" and false
// where it is none of them. It answers the caller's own word, never the field's text, so a caller
// that reads which of a closed set of words another party wrote - a reason, a state - decides on
// the field as it came and holds only its own word after, even where the rule's reading of the
// field is cut, as it is where the field shares a run with a credential the request carried. An
// empty field is none of words unless words holds "".
func (t RemoteText) OneOf(words ...string) (string, bool) {
	for _, word := range words {
		if t.Equal(word) {
			return word, true
		}
	}
	return "", false
}
