package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// bootDomainSigningKey is DISPATCH_SIGNING_KEY for every process this file starts, and the key the
// rows it seeds seal their refresh tokens under: a token sealed under another key would not open,
// and one stored in plain text the boot would retire before its check, so either would count as no
// token.
const bootDomainSigningKey = "boot-domain-signing-key"

// bootDomainRefusal is the log line the boot writes when it refuses its sign-in domain.
const bootDomainRefusal = `msg="dispatch: check the sign-in domain against the people"`

// bootDomainRow is a people row a case seeds, signed in with a sealed refresh token.
type bootDomainRow struct {
	email  string
	domain string // the Workspace domain the row records; empty records none
	kind   model.MemberKind
}

// seedBootDomainRows signs each row in through the people store, as a sign-in by this build does,
// so it holds a refresh token sealed under bootDomainSigningKey, a recorded Google account and the
// domain it names.
func seedBootDomainRows(t *testing.T, database *store.Store, rows ...bootDomainRow) {
	t.Helper()
	keys, err := auth.ParseSigningKeys(bootDomainSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	people := store.NewPgPeopleStore(database.Pool, keys, nil)
	for _, row := range rows {
		if err := people.SignInSubject(context.Background(), row.email, "sub-"+row.email, "refresh-"+row.email, row.domain, row.kind); err != nil {
			t.Fatalf("seed %s's sign-in recording %q: %v", row.email, row.domain, err)
		}
	}
}

// startBootDomainProcess starts the compiled envoy-dispatch on database under a Google sign-in
// for domain, against issuer in Google's place.
func startBootDomainProcess(t *testing.T, database *store.Store, issuer *oidctest.Issuer, domain string) *dispatchProcess {
	t.Helper()
	return startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		SigningKey: bootDomainSigningKey,
		Env: map[string]string{
			"DISPATCH_IDENTITY":                  "cookie",
			"DISPATCH_INSECURE_COOKIE":           "1",
			"DISPATCH_SIGNIN_ISSUER":             issuer.URL(),
			"DISPATCH_SIGNIN_CLIENT_ID":          cookieClientID,
			"DISPATCH_SIGNIN_CLIENT_SECRET":      cookieClientSecret,
			"DISPATCH_SIGNIN_DOMAIN":             domain,
			"DISPATCH_TEST_SIGNIN_DOMAIN_ISSUER": "1",
		},
		Without: []string{"DISPATCH_IDENTITY_HEADER_TRUSTED"},
	})
}

// requireBootRefused waits for the process to exit and fails unless it exited 1 refusing its
// sign-in domain, before it served, with an error naming each of names.
func requireBootRefused(t *testing.T, process *dispatchProcess, why string, names ...string) {
	t.Helper()
	process.WaitExit(t, "a boot that must refuse "+why)
	output := process.Output.String()
	if code := process.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("Dispatch with %s exited %d, want 1 refusing its sign-in domain:\n%s", why, code, output)
	}
	if !strings.Contains(output, bootDomainRefusal) {
		t.Fatalf("Dispatch with %s exited without refusing its sign-in domain:\n%s", why, output)
	}
	for _, name := range names {
		if !strings.Contains(output, name) {
			t.Errorf("the refusal of a boot with %s does not name %q:\n%s", why, name, output)
		}
	}
	if strings.Contains(output, process.listeningLine()) {
		t.Errorf("Dispatch with %s served before it refused:\n%s", why, output)
	}
}

// requireBootServes waits for the process to serve, and fails when it refused its sign-in domain.
func requireBootServes(t *testing.T, process *dispatchProcess, why string) {
	t.Helper()
	if err := process.healthyWithin(time.Minute); err != nil {
		t.Fatalf("Dispatch with %s did not start: %v", why, err)
	}
	if output := process.Output.String(); strings.Contains(output, bootDomainRefusal) {
		t.Fatalf("Dispatch with %s logged a refusal of its sign-in domain:\n%s", why, output)
	}
}

// Under DISPATCH_SIGNIN_DOMAIN, the compiled envoy-dispatch refuses to start while full members'
// rows that hold a refresh token record a Workspace domain and none records the setting, compared
// byte for byte, since each confirmation would then hold that member. It starts with an empty
// database, with the right domain, with full members' rows that record none, and with a restricted
// member's row alone; and after `envoy-dispatch forget-hosted-domains` it starts under a new
// domain it refused before.
func TestBootRefusesASignInDomainNoFullMemberRecords(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), cookieClientID, cookieClientSecret)
	members := []bootDomainRow{
		{email: "ada@example.com", domain: "example.com", kind: model.MemberFull},
		{email: "bob@example.com", domain: "example.com", kind: model.MemberFull},
	}

	t.Run("a mistyped domain", func(t *testing.T) {
		database := storetest.Open(t)
		seedBootDomainRows(t, database, members...)
		requireBootRefused(t, startBootDomainProcess(t, database, issuer, "exampel.com"),
			"DISPATCH_SIGNIN_DOMAIN=exampel.com over full members recording example.com",
			`\"exampel.com\"`, "record example.com", "envoy-dispatch forget-hosted-domains")
	})

	t.Run("a domain that differs in case alone", func(t *testing.T) {
		database := storetest.Open(t)
		seedBootDomainRows(t, database,
			bootDomainRow{email: "ada@example.com", domain: "Example.com", kind: model.MemberFull},
			bootDomainRow{email: "bob@example.com", domain: "Example.com", kind: model.MemberFull})
		requireBootRefused(t, startBootDomainProcess(t, database, issuer, "example.com"),
			"DISPATCH_SIGNIN_DOMAIN=example.com over full members recording Example.com",
			`\"example.com\"`, "record Example.com")
	})

	t.Run("an empty database", func(t *testing.T) {
		requireBootServes(t, startBootDomainProcess(t, storetest.Open(t), issuer, "exampel.com"), "an empty database")
	})

	t.Run("the right domain", func(t *testing.T) {
		database := storetest.Open(t)
		seedBootDomainRows(t, database, members...)
		process := startBootDomainProcess(t, database, issuer, "example.com")
		requireBootServes(t, process, "DISPATCH_SIGNIN_DOMAIN=example.com over full members recording it")
		census := `msg="dispatch: people census" checkable=2 held=0 uncheckable=0`
		if output := process.Output.String(); !strings.Contains(output, census) {
			t.Errorf("the boot did not log %s:\n%s", census, output)
		}
	})

	t.Run("full members recording no domain", func(t *testing.T) {
		database := storetest.Open(t)
		seedBootDomainRows(t, database,
			bootDomainRow{email: "ada@example.com", kind: model.MemberFull},
			bootDomainRow{email: "bob@example.com", kind: model.MemberFull})
		requireBootServes(t, startBootDomainProcess(t, database, issuer, "exampel.com"),
			"DISPATCH_SIGNIN_DOMAIN=exampel.com over full members recording no domain")
	})

	t.Run("a restricted member alone", func(t *testing.T) {
		database := storetest.Open(t)
		seedBootDomainRows(t, database, bootDomainRow{email: "dana@outside.example", domain: "outside.example", kind: model.MemberRestricted})
		requireBootServes(t, startBootDomainProcess(t, database, issuer, "example.com"),
			"DISPATCH_SIGNIN_DOMAIN=example.com over a restricted member recording outside.example")
	})

	t.Run("an intended change of domain", func(t *testing.T) {
		database := storetest.Open(t)
		seedBootDomainRows(t, database, members...)
		requireBootRefused(t, startBootDomainProcess(t, database, issuer, "new.example"),
			"DISPATCH_SIGNIN_DOMAIN=new.example before forget-hosted-domains", `\"new.example\"`, "record example.com")

		forget := exec.Command(cmdtest.Build(t, "envoy-dispatch"), "forget-hosted-domains")
		forget.Dir = t.TempDir()
		forget.Env = []string{"HOME=" + forget.Dir, "DATABASE_URL=" + database.Pool.Config().ConnString()}
		var output bytes.Buffer
		forget.Stdout, forget.Stderr = &output, &output
		if err := forget.Run(); err != nil || !strings.Contains(output.String(), "forget-hosted-domains: 2 people rows no longer record a Workspace domain") {
			t.Fatalf("envoy-dispatch forget-hosted-domains: %v, want exit 0 naming 2 rows:\n%s", err, output.String())
		}
		requireBootServes(t, startBootDomainProcess(t, database, issuer, "new.example"),
			"DISPATCH_SIGNIN_DOMAIN=new.example after forget-hosted-domains")
	})
}
