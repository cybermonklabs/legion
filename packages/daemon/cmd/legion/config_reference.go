package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// runConfigReference is `legion config-reference --json`: it prints the loader's key table
// (internal/config/keytable.go) as JSON and reads nothing — no legion.yaml, no daemon, no cluster
// — so a documentation build can run it anywhere. The output is one array, one object per
// top-level key in table order, each with its `name`, `default`, `required` and `description` and,
// under `nested`, every key of the mappings that belong to it, each with its own line, since the
// loader reads no nested key without one (TestNoKeyBypassesTheKeyTable). JSON is the one shape it
// prints, as `envoy-dispatch settings` prints one shape, so --json is required rather than a
// choice among formats.
func runConfigReference(_ context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "usage: legion config-reference --json"
	flags := newFlags("config-reference", usage, stderr)
	asJSON := flags.Bool("json", false, "print the key table as JSON, the one shape this command prints")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() > 0 || !*asJSON {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(config.ConfigReference()); err != nil {
		fmt.Fprintf(stderr, "legion config-reference: %v\n", err)
		return 1
	}
	return 0
}
