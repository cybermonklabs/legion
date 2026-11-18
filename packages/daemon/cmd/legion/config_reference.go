package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// newConfigReferenceCommand is `legion config reference --json`: it prints the loader's key table
// (internal/config/keytable.go) as JSON and reads nothing — no legion.yaml, no daemon, no cluster
// — so a documentation build can run it anywhere. The output is one array, one object per
// top-level key in table order, each with its `name`, `default`, `required` and `description` and,
// under `nested`, every key of the mappings that belong to it, each with its line, since the
// loader reads no nested key without one (TestNoKeyBypassesTheKeyTable). A line is a group's and
// not a mapping's, and one group serves four mappings: an item of the `items` of a Secret or a
// ConfigMap source, a volume's own or a projected one's, is read against the `secret.items[]` lines
// (config's readObjectSource), so the output prints those lines once, under that path, and the
// `items` lines of the ConfigMap and projected sources point there. JSON is the one shape it
// prints, as `envoy-dispatch settings` prints one shape, so --json is required rather than a
// choice among formats.
func newConfigReferenceCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reference --json",
		Short: "print legion.yaml's key table as JSON: every top-level key with its default, whether it is required and what it does, and the described nested keys under it",
		Args:  noArguments,
	}
	asJSON := cmd.Flags().Bool("json", false, "print the key table as JSON, the one shape this command prints")
	cmd.RunE = runs(func(context.Context, []string) int {
		if !*asJSON {
			return refuse(cmd, "--json is required: JSON is the one shape this command prints")
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(config.ConfigReference()); err != nil {
			fmt.Fprintf(stderr, "legion config reference: %v\n", err)
			return 1
		}
		return 0
	})
	return cmd
}
