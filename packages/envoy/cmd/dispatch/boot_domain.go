package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// checkPeopleAtBoot is the boot's one read of people under a Google sign-in rule (rule.Holds,
// which DISPATCH_SIGNIN_DOMAIN sets, with passkey sign-in on or off), after the boot retired the
// refresh tokens stored in plain text, so a retired row counts as one Dispatch cannot
// check. It logs the people census: the rows whose sign-in Dispatch can check (a refresh token
// stored and a Google account recorded), the rows with a recorded hold, and every other row.
//
// It refuses the boot when domain, DISPATCH_SIGNIN_DOMAIN as the settings trimmed it, is none of
// the Workspace domains that the full members' rows holding a refresh token record, while at least
// one such row records one. Each confirmation holds a full member whose ID token's hd is not the
// setting, so a mistyped domain would hold every member at their next hourly check. The domains
// are compared byte for byte, as oidc.WorkspaceLogin compares the hd claim. A row that records no
// domain does not count (it records one at its next confirmation), and neither does a restricted
// member's, whose hd is its own Workspace's or none. Under any other rule it reads nothing.
func checkPeopleAtBoot(ctx context.Context, people *store.PgPeopleStore, rule identity.SignInRule, domain string) error {
	if rule == nil || !rule.Holds() {
		return nil
	}
	census, err := people.BootPeople(ctx)
	if err != nil {
		return fmt.Errorf("read the people rows at boot: %w", err)
	}
	slog.Info("dispatch: people census", "checkable", census.Checkable, "held", census.Held, "uncheckable", census.Uncheckable)
	if domain == "" || len(census.FullMemberDomains) == 0 || slices.Contains(census.FullMemberDomains, domain) {
		return nil
	}
	return fmt.Errorf("DISPATCH_SIGNIN_DOMAIN %q matches no full member's sign-in, whose sign-ins record %s. "+
		"A mistyped domain would hold every member: correct the setting. "+
		"After an intended change of domain, run envoy-dispatch forget-hosted-domains once, then start Dispatch",
		domain, strings.Join(census.FullMemberDomains, ", "))
}
