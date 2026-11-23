// Package shellquote quotes words for a POSIX shell as Python's shlex does: the one place the
// envoy modules quote a word that goes into shell text, a command a person pastes or a script a
// program writes.
package shellquote

import "strings"

// Quote quotes s for a POSIX shell as Python's shlex.quote does: an empty pair of single quotes
// for an empty string, s unchanged when every character is in [A-Za-z0-9_@%+=:,./-], and
// otherwise s in single quotes, each ' in it written as '"'"'.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, unsafeInShell) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func unsafeInShell(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		return false
	}
	return !strings.ContainsRune("_@%+=:,./-", r)
}

// Join is Python's shlex.join: Quote over each argument, joined by single spaces.
func Join(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = Quote(arg)
	}
	return strings.Join(quoted, " ")
}
