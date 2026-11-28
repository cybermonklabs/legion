package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/sjawhar/legion/daemon/internal/modeltoken"
	"github.com/sjawhar/legion/daemon/internal/programbound"
)

// modelTokenTimeout bounds one sign-in, and stays under Oh My Pi's 10 s limit on an apiKey command.
// It is a variable so that this package's tests can give it their own deadline (programbound): a
// stand-in Cognito a loaded machine made slower than 8 s would fail the sign-in.
var modelTokenTimeout = 8 * time.Second

func init() {
	programbound.Register(&modelTokenTimeout)
}

// newModelTokenCommand is `legion model-token` (internal/modeltoken): it prints the access token
// alone on stdout, or exits 1 with the reason on stderr and nothing on stdout.
func newModelTokenCommand(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "model-token --region <region> --client-id <id> --username <user> --service-account-token-file <path> [--endpoint <url>]",
		Short:  "sign a pod in to Cognito with its service-account token and print the access token (a model apiKey command)",
		Hidden: true,
	}
	flags := cmd.Flags()
	var cfg modeltoken.Config
	flags.StringVar(&cfg.Region, "region", "", "the Cognito user pool's region")
	flags.StringVar(&cfg.ClientID, "client-id", "", "the Cognito app client the machine user signs in on")
	flags.StringVar(&cfg.Username, "username", "", "the Cognito machine user")
	flags.StringVar(&cfg.ServiceAccountTokenFile, "service-account-token-file", "", "the pod's projected service-account token, the custom challenge's answer")
	flags.StringVar(&cfg.Endpoint, "endpoint", "", "a Cognito endpoint in place of the region's own: a test's fake Cognito, or an override")
	cmd.RunE = runs(func(ctx context.Context, args []string) int {
		if len(args) > 0 {
			fmt.Fprintf(stderr, "legion model-token: unexpected argument %q\n", args[0])
			return 2
		}
		for _, name := range []string{"region", "client-id", "username", "service-account-token-file"} {
			if flags.Lookup(name).Value.String() == "" {
				fmt.Fprintf(stderr, "legion model-token: --%s is required\n", name)
				return 1
			}
		}
		signingIn, cancel := context.WithTimeout(ctx, programbound.Of(&modelTokenTimeout))
		defer cancel()
		token, err := modeltoken.Token(signingIn, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "legion model-token: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, token)
		return 0
	})
	return cmd
}
