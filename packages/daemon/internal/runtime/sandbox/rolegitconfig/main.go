// Command rolegitconfig prints the git configuration every process of a role gets,
// sandbox.GitConfigPairs, as the role's environment carries it (gitConfigEnv): GIT_CONFIG_COUNT,
// then GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n> for each pair, one NAME=value line each.
// scripts/e2e/pack-memory-job.sh runs it from packages/daemon with `go run
// ./internal/runtime/sandbox/rolegitconfig` and sets those variables on its Job's container, so the
// git the Job measures reads the role's own block, built from the checkout the harness runs from.
//
// Everything after a line's first `=` is the value, which may be empty or hold `!` and spaces. A
// key or a value holding a line break or another control character could not be read back as one
// line, so it is refused with exit 1, naming the pair, rather than printed.
package main

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

func main() {
	pairs := sandbox.GitConfigPairs()
	var block strings.Builder
	fmt.Fprintf(&block, "GIT_CONFIG_COUNT=%d\n", len(pairs))
	for i, pair := range pairs {
		for _, text := range pair {
			if strings.ContainsFunc(text, unicode.IsControl) {
				fmt.Fprintf(os.Stderr, "rolegitconfig: pair %d (%q = %q) holds a control character, so it cannot be printed as one line\n", i, pair[0], pair[1])
				os.Exit(1)
			}
		}
		fmt.Fprintf(&block, "GIT_CONFIG_KEY_%d=%s\nGIT_CONFIG_VALUE_%d=%s\n", i, pair[0], i, pair[1])
	}
	if _, err := os.Stdout.WriteString(block.String()); err != nil {
		fmt.Fprintf(os.Stderr, "rolegitconfig: %v\n", err)
		os.Exit(1)
	}
}
