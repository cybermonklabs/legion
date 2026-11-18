// Package envlisting reads the environment a child process printed with env(1), for the tests
// that check what the daemon passes the commands it starts.
//
// env prints one variable to a line, in the order its environment holds them, and the shell a
// command runs under can reorder that environment first: dash writes a child's environment from
// its own variable table, so a variable the daemon appends last can be the first line. And a
// listing holds variables the test does not control, PWD among them, whose values can contain any
// word. So a check reads each variable whole, by name or by its whole value, and never searches
// the listing's text.
package envlisting

import "strings"

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

// HoldingValue returns the name of a variable of vars whose whole value is one of values, and
// whether there is one.
func HoldingValue(vars map[string]string, values ...string) (string, bool) {
	for name, value := range vars {
		for _, want := range values {
			if value == want {
				return name, true
			}
		}
	}
	return "", false
}
