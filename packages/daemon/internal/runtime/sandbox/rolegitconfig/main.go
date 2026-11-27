// Command rolegitconfig prints the git configuration every process of a role gets,
// sandbox.GitConfigPairs, as the role's environment carries it (sandbox.GitConfigEnv):
// GIT_CONFIG_COUNT, then GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n> for each pair, one NAME=value
// line each. scripts/e2e/pack-memory-job.sh runs it from packages/daemon with `go run
// ./internal/runtime/sandbox/rolegitconfig` and sets those variables on its Job's container, so the
// git the Job measures reads the role's own block, built from the checkout the harness runs from.
//
// Everything after a line's first `=` is the value, which may be empty or hold `!` and spaces. A
// key or a value holding a line break or another control character could not be read back as one
// line, so it is refused with exit 1, naming the variable, rather than printed.
package main

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

func main() {
	var block strings.Builder
	for _, variable := range sandbox.GitConfigEnv() {
		if strings.ContainsFunc(variable.Value, unicode.IsControl) {
			fmt.Fprintf(os.Stderr, "rolegitconfig: %s (%q) holds a control character, so it cannot be printed as one line\n", variable.Name, variable.Value)
			os.Exit(1)
		}
		fmt.Fprintf(&block, "%s=%s\n", variable.Name, variable.Value)
	}
	if _, err := os.Stdout.WriteString(block.String()); err != nil {
		fmt.Fprintf(os.Stderr, "rolegitconfig: %v\n", err)
		os.Exit(1)
	}
}
