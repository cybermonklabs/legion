// Package envlisting reads the environment a child process printed with env(1), for the tests
// that check what the daemon passes the commands it starts.
//
// env prints one variable to a line, in the order its environment holds them, and the shell a
// command runs under can reorder that environment first: dash writes a child's environment from
// its own variable table, so a variable the daemon appends last can be the first line. And a
// listing holds variables the test does not control, PWD and TMPDIR among them, whose values can
// contain any word. So a check reads each variable whole, by name, and looks for a value the test
// planted only when that value is Unique: no machine's path can hold a value made after it.
package envlisting

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// Vars returns the variables of an env listing, by name.
func Vars(listing string) map[string]string {
	vars := make(map[string]string)
	for _, line := range strings.Split(listing, "\n") {
		if name, value, ok := strings.Cut(line, "="); ok && name != "" {
			vars[name] = value
		}
	}
	return vars
}

// Unique returns prefix followed by 16 random hex digits: a value a test plants in an
// environment and then looks for, which no other variable can hold by chance.
func Unique(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

// Carrying returns the name of a variable of vars whose value contains one of values, and
// whether there is one. Each value should be Unique, so that only a variable the planted value
// reached can match, inside a longer value (a URL, a list) as well.
func Carrying(vars map[string]string, values ...string) (string, bool) {
	for name, value := range vars {
		for _, planted := range values {
			if strings.Contains(value, planted) {
				return name, true
			}
		}
	}
	return "", false
}
