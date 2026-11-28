package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
)

// revokeDispatchSignInsCommand is the subcommand that ends every Dispatch sign-in the broker has
// issued: step 3 of rolling BROKER_DISPATCH_SIGNIN back
// (docs/site/src/content/docs/broker/operate.md, config.Config.DispatchSignIn).
const revokeDispatchSignInsCommand = "revoke-dispatch-signins"

// revokeDispatchSignIns runs enroll.Service.RevokeDispatchSignIns, which says what it revokes and
// under whose name, and prints how many it revoked. It reads no broker setting but
// BROKER_DATABASE_URL (config.LoadDatabase), and signs in to that database as the server does
// (openStore): by RDS IAM token from the process's AWS identity for an IAM URL, and otherwise with
// the URL's password, PGPASSWORD or a passfile. It binds no listener and migrates nothing, as the
// end-sessions subcommand does not: an image older than the servers must not migrate the database
// behind them. A failure part way prints how many it revoked before it and exits 1; a run that
// revokes every sign-in, none included, exits 0.
func revokeDispatchSignIns(ctx context.Context, stdout, stderr io.Writer) int {
	cfg, err := config.LoadDatabase(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", revokeDispatchSignInsCommand, err)
		return 1
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", revokeDispatchSignInsCommand, err)
		return 1
	}
	defer st.Close()
	revoked, err := (&enroll.Service{Store: st}).RevokeDispatchSignIns(ctx)
	fmt.Fprintf(stdout, "%s: revoked %s\n", revokeDispatchSignInsCommand, counted(revoked, "Dispatch sign-in"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", revokeDispatchSignInsCommand, err)
		return 1
	}
	return 0
}
