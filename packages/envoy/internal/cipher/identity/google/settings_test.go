package google_test

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
)

// exampleVMServiceAccount is a placeholder Google service account; this repository is public, so
// no real account, project or instance name appears in a test.
const exampleVMServiceAccount = "example-fleet@example-project.iam.gserviceaccount.com"

// getenv reads e, as the broker's own environment reads a variable it was not given: "".
func getenv(e map[string]string) func(string) string {
	return func(name string) string { return e[name] }
}

// validVMEnv is the environment with one VM service fully configured: the Google audience, the
// service's account, its project and its instance-name pattern.
func validVMEnv() map[string]string {
	return map[string]string{
		"BROKER_GOOGLE_OIDC_AUDIENCE": "example-broker",
		"BROKER_VM_SERVICES":          "example-service=" + exampleVMServiceAccount,
		"BROKER_VM_PROJECTS":          exampleVMServiceAccount + "=example-project",
		"BROKER_VM_INSTANCES":         exampleVMServiceAccount + "=example-vm-*",
	}
}

// servicesByAccount is the service each listed account is named for, the map
// Config.ServicesByAccount holds.
func servicesByAccount(c google.Config) map[string]string { return c.ServicesByAccount }

// TestLoadReadsTheVMServices pins the four VM settings: one entry gives the service, the project
// and the instance pattern, each keyed by the account's email.
func TestLoadReadsTheVMServices(t *testing.T) {
	c, err := google.LoadConfig(getenv(validVMEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Audience != "example-broker" {
		t.Fatalf("Audience = %q, want example-broker", c.Audience)
	}
	if want := map[string]string{exampleVMServiceAccount: "example-service"}; !maps.Equal(servicesByAccount(c), want) {
		t.Fatalf("ServicesByAccount = %v, want %v", servicesByAccount(c), want)
	}
	pinned := []google.GoogleEntry{{Account: exampleVMServiceAccount, Service: "example-service", CloudProject: "example-project", Instance: "example-vm-*"}}
	if !slices.Equal(c.Machines, pinned) {
		t.Fatalf("Machines = %+v, want %+v: the two pins are read into the entry and nowhere else", c.Machines, pinned)
	}

	// Two accounts for one service, entries separated by any white space, and an audience with
	// white space around it, which is trimmed, as a token's audience never carries it.
	const second = "second-fleet@example-project.iam.gserviceaccount.com"
	two := validVMEnv()
	two["BROKER_GOOGLE_OIDC_AUDIENCE"] = " \texample-broker\n"
	two["BROKER_VM_SERVICES"] += "\texample-service=" + second
	two["BROKER_VM_PROJECTS"] += "\n" + second + "=example-other"
	two["BROKER_VM_INSTANCES"] += "\t" + second + "=example-vm-[0-9]*"
	c, err = google.LoadConfig(getenv(two))
	if err != nil {
		t.Fatal(err)
	}
	if c.Audience != "example-broker" {
		t.Fatalf("Audience = %q, want example-broker with its white space trimmed", c.Audience)
	}
	if want := map[string]string{exampleVMServiceAccount: "example-service", second: "example-service"}; !maps.Equal(servicesByAccount(c), want) {
		t.Fatalf("ServicesByAccount with two accounts = %v, want %v", servicesByAccount(c), want)
	}
	bothPinned := []google.GoogleEntry{
		{Account: exampleVMServiceAccount, Service: "example-service", CloudProject: "example-project", Instance: "example-vm-*"},
		{Account: second, Service: "example-service", CloudProject: "example-other", Instance: "example-vm-[0-9]*"},
	}
	if !slices.Equal(c.Machines, bothPinned) {
		t.Fatalf("Machines with two accounts = %+v, want %+v", c.Machines, bothPinned)
	}
	// A second account pinned in one setting and not the other is half a pin, refused naming
	// both settings and that account, not only the first's.
	for _, missing := range []string{"BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES"} {
		e := maps.Clone(two)
		e[missing] = validVMEnv()[missing]
		named := "BROKER_VM_INSTANCES"
		if missing == "BROKER_VM_INSTANCES" {
			named = "BROKER_VM_PROJECTS"
		}
		_, err := google.LoadConfig(getenv(e))
		want := named + " names " + second + " but " + missing + ` does not, got "example-service=` + second + `"`
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("LoadConfig with the second account missing from %s = %v, want a refusal starting %q", missing, err, want)
		}
	}
}

// TestTheGoogleIssuerDefaultsToGooglesOwn: BROKER_GOOGLE_OIDC_ISSUER is Google's own issuer when
// the environment names none, and a dev stack's stand-in when it does, which is the only reason
// the variable exists.
func TestTheGoogleIssuerDefaultsToGooglesOwn(t *testing.T) {
	c, err := google.LoadConfig(getenv(validVMEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Issuer != google.DefaultIssuer || google.DefaultIssuer != "https://accounts.google.com" {
		t.Fatalf("Issuer = %q, want %q", c.Issuer, google.DefaultIssuer)
	}
	e := validVMEnv()
	e["BROKER_GOOGLE_OIDC_ISSUER"] = " http://127.0.0.1:8080\n"
	if c, err = google.LoadConfig(getenv(e)); err != nil || c.Issuer != "http://127.0.0.1:8080" {
		t.Fatalf("Issuer with a stand-in issuer = %q, %v; want the issuer with its white space trimmed", c.Issuer, err)
	}
	// With no Google audience the issuer is still answered, since nothing reads it then: it is
	// the audience that turns Google identity tokens on at all.
	if c, err = google.LoadConfig(getenv(nil)); err != nil || c.Issuer != google.DefaultIssuer {
		t.Fatalf("Issuer with no Google audience = %q, %v; want the default", c.Issuer, err)
	}
}

// TestTheVMExchangeAccountsGiveAFleetItsPersonAndProject pins BROKER_VM_EXCHANGE_ACCOUNTS: a
// listed account it names writes that project, acts for that person and may fix its session,
// while a listed account it does not name is its service and nothing else. Its refusals are each
// its own: an entry that is not of the form, a blank field, an account no VM service lists, an
// account named twice, an account no VM setting may name, and the setting with no Google
// audience.
func TestTheVMExchangeAccountsGiveAFleetItsPersonAndProject(t *testing.T) {
	const second = "second-fleet@example-project.iam.gserviceaccount.com"
	e := validVMEnv()
	e["BROKER_VM_SERVICES"] += " example-other=" + second
	e["BROKER_VM_PROJECTS"] += " " + second + "=example-other"
	e["BROKER_VM_INSTANCES"] += " " + second + "=example-vm-*"
	e["BROKER_VM_EXCHANGE_ACCOUNTS"] = exampleVMServiceAccount + "=EXAMPLE=ada@example.com=example-fleet-1"
	c, err := google.LoadConfig(getenv(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []google.GoogleEntry{
		{
			Account: exampleVMServiceAccount, Service: "example-service",
			CloudProject: "example-project", Instance: "example-vm-*",
			Project: "EXAMPLE", Person: "ada@example.com", Session: "example-fleet-1",
		},
		{
			Account: second, Service: "example-other",
			CloudProject: "example-other", Instance: "example-vm-*",
		},
	}
	if !slices.Equal(c.Machines, want) {
		t.Fatalf("Machines = %+v, want %+v", c.Machines, want)
	}
	// An entry that fixes no session: everything after the third = is the session, so there is
	// none to read.
	noSession := validVMEnv()
	noSession["BROKER_VM_EXCHANGE_ACCOUNTS"] = exampleVMServiceAccount + "=EXAMPLE=ada@example.com"
	c, err = google.LoadConfig(getenv(noSession))
	if err != nil || len(c.Machines) != 1 || c.Machines[0].Session != "" || c.Machines[0].Person != "ada@example.com" {
		t.Fatalf("Machines with no session fixed = %+v, %v", c.Machines, err)
	}
	for _, row := range []struct{ value, want string }{
		{exampleVMServiceAccount + "=EXAMPLE", "BROKER_VM_EXCHANGE_ACCOUNTS must be <service account email>=<project>=<person>[=<session>] entries"},
		{exampleVMServiceAccount + "==ada@example.com", "BROKER_VM_EXCHANGE_ACCOUNTS must be <service account email>=<project>=<person>[=<session>] entries"},
		{exampleVMServiceAccount + "=EXAMPLE=", "BROKER_VM_EXCHANGE_ACCOUNTS must be <service account email>=<project>=<person>[=<session>] entries"},
		{"not-an-email=EXAMPLE=ada@example.com", "BROKER_VM_EXCHANGE_ACCOUNTS must name user-managed service accounts"},
		{"system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com", "BROKER_VM_EXCHANGE_ACCOUNTS must name user-managed service accounts"},
		{exampleVMServiceAccount + "=EXAMPLE=ada@example.com " + exampleVMServiceAccount + "=OTHER=bob@example.com",
			"BROKER_VM_EXCHANGE_ACCOUNTS names the service account " + exampleVMServiceAccount + " twice"},
		{second + "=EXAMPLE=ada@example.com", "BROKER_VM_EXCHANGE_ACCOUNTS names " + second},
	} {
		bad := validVMEnv()
		bad["BROKER_VM_EXCHANGE_ACCOUNTS"] = row.value
		_, err := google.LoadConfig(getenv(bad))
		if err == nil || !strings.HasPrefix(err.Error(), row.want) {
			t.Errorf("LoadConfig(BROKER_VM_EXCHANGE_ACCOUNTS=%q) = %v, want a refusal starting %q", row.value, err, row.want)
		}
	}
	_, err = google.LoadConfig(getenv(map[string]string{"BROKER_VM_EXCHANGE_ACCOUNTS": exampleVMServiceAccount + "=EXAMPLE=ada@example.com"}))
	if want := "BROKER_VM_EXCHANGE_ACCOUNTS needs BROKER_GOOGLE_OIDC_AUDIENCE"; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("LoadConfig with the fleet grants and no Google audience = %v, want a refusal starting %q", err, want)
	}
}

// jobList is the job list exchange.ParseJobProjects reads from text.
func jobList(t *testing.T, text string) exchange.JobProjects {
	t.Helper()
	job, err := exchange.ParseJobProjects(text)
	if err != nil {
		t.Fatalf("exchange.ParseJobProjects(%q): %v", text, err)
	}
	return job
}

// TestTheVMExchangeJobAccountsGiveAFleetAJobsProjects pins BROKER_VM_EXCHANGE_JOB_ACCOUNTS: a
// listed account it names lands in its own machine entry, beside its service and its pins, with
// its projects as Job and the session it fixes, and no person and no project. Its refusals are
// config.Load's, word for word: an entry not of the form, projects of any other spelling, an
// account named twice, an account no VM service lists, an account BROKER_VM_EXCHANGE_ACCOUNTS
// names too, and the setting with no Google audience. Mentions names the job's list under its own
// setting, which config's BROKER_POD_OPERATORS check reads.
func TestTheVMExchangeJobAccountsGiveAFleetAJobsProjects(t *testing.T) {
	const second = "second-fleet@example-project.iam.gserviceaccount.com"
	withSecond := func() map[string]string {
		e := validVMEnv()
		e["BROKER_VM_SERVICES"] += " example-other=" + second
		return e
	}
	e := withSecond()
	e["BROKER_VM_EXCHANGE_JOB_ACCOUNTS"] = exampleVMServiceAccount + "=EXAMPLE=example-fleet-job " + second + "=public,PRIVA"
	c, err := google.LoadConfig(getenv(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []google.GoogleEntry{
		{
			Account: exampleVMServiceAccount, Service: "example-service",
			CloudProject: "example-project", Instance: "example-vm-*",
			Job: jobList(t, "EXAMPLE"), Session: "example-fleet-job",
		},
		{Account: second, Service: "example-other", Job: jobList(t, "public,PRIVA")},
	}
	if !slices.Equal(c.Machines, want) {
		t.Fatalf("Machines = %+v, want %+v", c.Machines, want)
	}
	mentions := c.Mentions(identity.Account{Kind: google.Kind, Address: second})
	if want := []identity.Mention{{Variable: "BROKER_VM_EXCHANGE_JOB_ACCOUNTS", Service: "example-other", Job: jobList(t, "public,PRIVA")}}; !slices.Equal(mentions, want) {
		t.Errorf("Mentions(%s) = %+v, want %+v", second, mentions, want)
	}
	for _, row := range []struct{ value, want string }{
		{exampleVMServiceAccount, "BROKER_VM_EXCHANGE_JOB_ACCOUNTS must be <service account email>=<projects>[=<session>] entries, the session everything after the second =, got " + strconv.Quote(exampleVMServiceAccount)},
		{exampleVMServiceAccount + "=acme", "BROKER_VM_EXCHANGE_JOB_ACCOUNTS names projects no job entry may name, got " + strconv.Quote(exampleVMServiceAccount+"=acme")},
		{"123456789-compute@developer.gserviceaccount.com=EXAMPLE", "BROKER_VM_EXCHANGE_JOB_ACCOUNTS names a project's default service account"},
		{exampleVMServiceAccount + "=EXAMPLE " + exampleVMServiceAccount + "=public",
			"BROKER_VM_EXCHANGE_JOB_ACCOUNTS names the service account " + exampleVMServiceAccount + " twice, got " + strconv.Quote(exampleVMServiceAccount+"=public") + " again"},
		{"third-fleet@example-project.iam.gserviceaccount.com=EXAMPLE",
			`BROKER_VM_EXCHANGE_JOB_ACCOUNTS names third-fleet@example-project.iam.gserviceaccount.com, got "third-fleet@example-project.iam.gserviceaccount.com=EXAMPLE", but BROKER_VM_SERVICES does not list it; a job's projects are given to a listed VM account`},
	} {
		bad := withSecond()
		bad["BROKER_VM_EXCHANGE_JOB_ACCOUNTS"] = row.value
		if _, err := google.LoadConfig(getenv(bad)); err == nil || !strings.HasPrefix(err.Error(), row.want) {
			t.Errorf("LoadConfig(BROKER_VM_EXCHANGE_JOB_ACCOUNTS=%q) = %v, want a refusal starting %q", row.value, err, row.want)
		}
	}
	both := withSecond()
	both["BROKER_VM_EXCHANGE_ACCOUNTS"] = exampleVMServiceAccount + "=EXAMPLE=ada@example.com"
	both["BROKER_VM_EXCHANGE_JOB_ACCOUNTS"] = exampleVMServiceAccount + "=EXAMPLE"
	wantBoth := "BROKER_VM_EXCHANGE_JOB_ACCOUNTS and BROKER_VM_EXCHANGE_ACCOUNTS both name the service account " + exampleVMServiceAccount +
		`, got "` + exampleVMServiceAccount + `=EXAMPLE" and "` + exampleVMServiceAccount + `=EXAMPLE=ada@example.com"; one account has one answer, a token that acts for its person or a job's that names none, so name it in one of them`
	if _, err := google.LoadConfig(getenv(both)); err == nil || err.Error() != wantBoth {
		t.Errorf("LoadConfig with an account both settings name = %v, want %q", err, wantBoth)
	}
	_, err = google.LoadConfig(getenv(map[string]string{"BROKER_VM_EXCHANGE_JOB_ACCOUNTS": exampleVMServiceAccount + "=EXAMPLE"}))
	if want := "BROKER_VM_EXCHANGE_JOB_ACCOUNTS needs BROKER_GOOGLE_OIDC_AUDIENCE, whose Google identity tokens it configures"; err == nil || err.Error() != want {
		t.Errorf("LoadConfig with a job grant and no Google audience = %v, want %q", err, want)
	}
}

// TestAVMAccountIsPinnedToBothOrToNeither pins the rule each pin follows: an account named in
// BROKER_VM_PROJECTS and in BROKER_VM_INSTANCES is held to that project and those names, an
// account named in neither takes its machines wherever they run, and an account named in one and
// not the other is refused at boot naming both settings and the account, since half a pin says
// nothing clear. It also pins that neither setting may name an account no VM service lists (the
// first such entry as written is the one named), an account twice, or a malformed value; and that
// an instance glob holds only an instance name's characters and path.Match's.
func TestAVMAccountIsPinnedToBothOrToNeither(t *testing.T) {
	for _, missing := range []string{"BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES"} {
		named := "BROKER_VM_INSTANCES"
		if missing == "BROKER_VM_INSTANCES" {
			named = "BROKER_VM_PROJECTS"
		}
		e := validVMEnv()
		delete(e, missing)
		_, err := google.LoadConfig(getenv(e))
		want := named + " names " + exampleVMServiceAccount + " but " + missing + ` does not, got "example-service=` + exampleVMServiceAccount + `"`
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("LoadConfig with %s unset = %v, want a refusal starting %q", missing, err, want)
		}
	}
	// Neither pin: the account is listed, and its entry takes a machine wherever it runs.
	unpinned := validVMEnv()
	delete(unpinned, "BROKER_VM_PROJECTS")
	delete(unpinned, "BROKER_VM_INSTANCES")
	c, err := google.LoadConfig(getenv(unpinned))
	if err != nil {
		t.Fatalf("LoadConfig with neither pin = %v, want an unpinned account", err)
	}
	// Neither pin reaches the entry, which is the only place a pin is read.
	want := []google.GoogleEntry{{Account: exampleVMServiceAccount, Service: "example-service"}}
	if !slices.Equal(c.Machines, want) {
		t.Errorf("Machines with neither pin = %+v, want %+v", c.Machines, want)
	}
	const other, third = "other-fleet@example-project.iam.gserviceaccount.com", "third-fleet@example-project.iam.gserviceaccount.com"
	for name, values := range map[string][2]string{
		"BROKER_VM_PROJECTS":  {"example-project", "example-project"},
		"BROKER_VM_INSTANCES": {"example-vm-*", "example-vm-*"},
	} {
		// Twenty loads, since a refusal that ranged over a map would name either account.
		for range 20 {
			e := validVMEnv()
			e[name] += " " + other + "=" + values[0] + " " + third + "=" + values[1]
			_, err := google.LoadConfig(getenv(e))
			want := name + " names " + other + `, got "` + other + "=" + values[0] + `", but BROKER_VM_SERVICES does not list it`
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("LoadConfig with %s naming two unlisted accounts = %v, want a refusal starting %q", name, err, want)
			}
		}
	}
	for _, row := range []struct{ name, value string }{
		{"BROKER_VM_PROJECTS", "Example_Project"},
		{"BROKER_VM_PROJECTS", "123456789012"},
		{"BROKER_VM_PROJECTS", "example-project=x"},
		{"BROKER_VM_INSTANCES", "example-vm-["},
		{"BROKER_VM_INSTANCES", "[!t]*"},
		{"BROKER_VM_INSTANCES", "example-vm-*/"},
		{"BROKER_VM_INSTANCES", "Example-VM-*"},
		{"BROKER_VM_INSTANCES", "example-vm-*=x"},
		{"BROKER_VM_INSTANCES", "example.vm.*"},
		{"BROKER_VM_INSTANCES", "example_vm_*"},
		{"BROKER_VM_INSTANCES", "a{b,c}"},
		{"BROKER_VM_INSTANCES", `example-vm-\*`},
		{"BROKER_VM_INSTANCES", "example-vm^x"},
		{"BROKER_VM_INSTANCES", "^example-vm-*"},
		{"BROKER_VM_INSTANCES", ""},
		{"BROKER_VM_PROJECTS", ""},
		{"BROKER_VM_PROJECTS", "short"},
		{"BROKER_VM_PROJECTS", strings.Repeat("a", 31)},
		{"BROKER_VM_PROJECTS", "example-project-"},
	} {
		e := validVMEnv()
		bad := exampleVMServiceAccount + "=" + row.value
		e[row.name] = bad
		_, err := google.LoadConfig(getenv(e))
		if err == nil || !strings.HasPrefix(err.Error(), row.name+" must be <service account email>=<") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("LoadConfig(%s=%q) = %v, want a refusal naming it and the entry", row.name, bad, err)
		}
	}
	for _, glob := range []string{"example-vm-[0-9]*", "example-vm-[^x]?", "*"} {
		e := validVMEnv()
		e["BROKER_VM_INSTANCES"] = exampleVMServiceAccount + "=" + glob
		c, err := google.LoadConfig(getenv(e))
		if err != nil || len(c.Machines) != 1 || c.Machines[0].Instance != glob {
			t.Errorf("LoadConfig(BROKER_VM_INSTANCES glob %q) = %+v, %v; want it accepted", glob, c.Machines, err)
		}
	}
	for _, setting := range []string{"BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES"} {
		e := validVMEnv()
		bad := "not-an-email=" + strings.SplitN(validVMEnv()[setting], "=", 2)[1]
		e[setting] = bad
		_, err := google.LoadConfig(getenv(e))
		if want := setting + " must name user-managed service accounts"; err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("LoadConfig(%s=%q) = %v, want a refusal starting %q and naming the entry", setting, bad, err, want)
		}
	}
	for setting, row := range map[string]struct {
		kind   string
		values []string
	}{
		"BROKER_VM_PROJECTS":  {"project id", []string{"example-other", "example-project"}},
		"BROKER_VM_INSTANCES": {"instance-name glob", []string{"example-other", "example-vm-*"}},
	} {
		kind := row.kind
		for _, other := range row.values {
			e := validVMEnv()
			e[setting] += " " + exampleVMServiceAccount + "=" + other
			_, err := google.LoadConfig(getenv(e))
			want := setting + " names the service account " + exampleVMServiceAccount + " twice; each account has one " + kind + ", so name it once"
			if err == nil || err.Error() != want {
				t.Errorf("LoadConfig with an account given twice in %s = %v, want %q", setting, err, want)
			}
		}
	}
}

// TestAKubernetesSubjectIsRefusedAsAVMAccount pins half of the rule that the two account kinds
// never overlap: BROKER_VM_SERVICES refuses a Kubernetes subject, so a pod's account cannot be
// bound as a VM service. The other half, BROKER_SERVICE_LOGINS refusing a Google email, is
// config's (TestAnAccountBoundByTheKubernetesServicesIsRefusedAsAVMAccount).
func TestAKubernetesSubjectIsRefusedAsAVMAccount(t *testing.T) {
	const subject = "system:serviceaccount:example:example-sa"
	e := validVMEnv()
	e["BROKER_VM_SERVICES"] = "example-service=" + subject
	e["BROKER_VM_PROJECTS"] = subject + "=example-project"
	e["BROKER_VM_INSTANCES"] = subject + "=example-vm-*"
	_, err := google.LoadConfig(getenv(e))
	if want := `BROKER_VM_SERVICES must name user-managed service accounts, <account id>@<project id>.iam.gserviceaccount.com, got "example-service=` + subject + `"`; err == nil || err.Error() != want {
		t.Fatalf("LoadConfig with a Kubernetes subject in BROKER_VM_SERVICES = %v, want %q", err, want)
	}
}

// TestWithNoGoogleAudienceEveryVMFieldIsEmpty pins requirement R4's configuration half: with no
// Google audience and no VM setting, every VM field is empty, so nothing downstream can act on one.
func TestWithNoGoogleAudienceEveryVMFieldIsEmpty(t *testing.T) {
	c, err := google.LoadConfig(getenv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Audience != "" || servicesByAccount(c) != nil {
		t.Fatalf("VM configuration = audience %q, services %v; want empty with no Google audience", c.Audience, servicesByAccount(c))
	}
	if c.Machines != nil {
		t.Fatalf("Machines = %+v, want none with no Google audience: the exchange then registers no Google subject token type", c.Machines)
	}
}
