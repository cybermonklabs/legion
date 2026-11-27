package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"google.golang.org/api/option"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// directoryCheckUsage is what `envoy-dispatch directory-check` prints for arguments it cannot take.
const directoryCheckUsage = "usage: envoy-dispatch directory-check --active <login> --held <login> --scope-probe <scope>"

// directoryCheckRefusal is the OAuth error code Google's token endpoint answers a JWT whose scope
// the signer's Domain-wide Delegation grant does not give.
const directoryCheckRefusal = "unauthorized_client"

// directoryCheck runs `envoy-dispatch directory-check --active <login> --held <login> --scope-probe
// <scope>`, the IAP deployment operator's probe of Dispatch's Workspace Directory reads, made
// before IAP admits anyone. It builds the reader Dispatch builds from DISPATCH_DIRECTORY_ACCOUNT
// and DISPATCH_DIRECTORY_ADMIN (identity.DirectoryReader), checks each login once as a person with
// no stored Directory user id, so by its primary address, and prints `<login>: Active`, `Held` or
// `Failed (<reason>)`. Then it asks Google for a token for the probe scope with the same signer and
// delegated user, and prints `scope <scope>: granted`, `refused (<OAuth error code>)`, or
// `Failed (<reason>)` when Google gave neither answer. Each check and the probe has
// identity.DirectoryFlight, as a check in the server has. A reason names a class and an HTTP
// status, never Google's words.
//
// It exits 0 only when the --active login is Active, the --held login is Held, and the probe is
// refused with unauthorized_client: the grant reads users and gives no other scope. It exits 1 for
// any other verdict, or when the reader cannot be built (no Application Default Credentials), and
// 2 for arguments it cannot take or either setting unset. It runs as its own process, with no
// server and no database. opts reach Google; a deployment passes none.
func directoryCheck(ctx context.Context, args []string, env settingValues, stdout, stderr io.Writer, opts ...option.ClientOption) int {
	flags := flag.NewFlagSet("directory-check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	active := flags.String("active", "", "a login whose Workspace account is active, by its primary address: it must check Active")
	held := flags.String("held", "", "a login whose Workspace account is suspended, archived or deleted: it must check Held")
	probe := flags.String("scope-probe", "", "a scope the Domain-wide Delegation grant must not give: Google must refuse it with "+directoryCheckRefusal)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	activeLogin, heldLogin, probeScope := model.CanonicalLogin(*active), model.CanonicalLogin(*held), strings.TrimSpace(*probe)
	if flags.NArg() != 0 || activeLogin == "" || heldLogin == "" || probeScope == "" {
		fmt.Fprintln(stderr, directoryCheckUsage)
		return 2
	}
	signer, admin := strings.TrimSpace(env.get("DISPATCH_DIRECTORY_ACCOUNT")), strings.TrimSpace(env.get("DISPATCH_DIRECTORY_ADMIN"))
	for _, setting := range []struct{ name, value string }{{"DISPATCH_DIRECTORY_ACCOUNT", signer}, {"DISPATCH_DIRECTORY_ADMIN", admin}} {
		name, value := setting.name, setting.value
		if value == "" {
			fmt.Fprintf(stderr, "directory-check: %s is not set\n", name)
			return 2
		}
	}
	reader, err := identity.NewDirectoryReader(ctx, signer, admin, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "directory-check: %v\n", err)
		return 1
	}
	code := 0
	for _, login := range []struct {
		email string
		want  identity.DirectoryOutcome
	}{{activeLogin, identity.DirectoryActive}, {heldLogin, identity.DirectoryHeld}} {
		checkCtx, cancel := context.WithTimeout(ctx, identity.DirectoryFlight)
		outcome, _, err := reader.Check(checkCtx, login.email, "")
		cancel()
		verdict := outcome.String()
		if err != nil {
			verdict = "Failed (" + directoryFailure(err) + ")"
		}
		fmt.Fprintf(stdout, "%s: %s\n", login.email, verdict)
		if outcome != login.want {
			fmt.Fprintf(stderr, "directory-check: %s checked %s, want %s\n", login.email, outcome, login.want)
			code = 1
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, identity.DirectoryFlight)
	granted, refusal, err := reader.ProbeScope(probeCtx, probeScope)
	cancel()
	switch {
	case err != nil:
		fmt.Fprintf(stdout, "scope %s: Failed (%s)\n", probeScope, directoryFailure(err))
	case granted:
		fmt.Fprintf(stdout, "scope %s: granted\n", probeScope)
	default:
		fmt.Fprintf(stdout, "scope %s: refused (%s)\n", probeScope, refusal)
	}
	if err != nil || granted || refusal != directoryCheckRefusal {
		fmt.Fprintf(stderr, "directory-check: Google must refuse scope %s with %s: the grant gives the Directory's read scope alone\n", probeScope, directoryCheckRefusal)
		code = 1
	}
	return code
}

// directoryFailure is a failed check's reason: its class and HTTP status.
func directoryFailure(err error) string {
	var failed *identity.DirectoryError
	if errors.As(err, &failed) {
		return failed.Reason()
	}
	return "unclassified"
}
