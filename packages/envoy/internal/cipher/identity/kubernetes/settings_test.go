package kubernetes_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
)

// getenv reads e, as the broker's own environment reads a variable it was not given: "".
func getenv(e map[string]string) func(string) string {
	return func(name string) string { return e[name] }
}

// withK8sIssuer is the environment with the cluster's issuer and audience set, the pair every
// setting about pods needs; withExchange adds the exchange audience.
func withK8sIssuer() map[string]string {
	return map[string]string{
		"BROKER_K8S_OIDC_ISSUER":   "https://cluster.example",
		"BROKER_K8S_OIDC_AUDIENCE": "dispatch-broker",
	}
}

func withExchange() map[string]string {
	e := withK8sIssuer()
	e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch-exchange"
	return e
}

// TestLoadReadsTheExchangeAccounts pins BROKER_EXCHANGE_ACCOUNTS: whitespace-separated
// <account>=<project>=<person>[=<session>] entries in order, none when unset, a session that may
// itself carry "=", an empty project (the shared public-session account's entry, whose named
// tokens name no project), an empty project and person (an account whose tokens act as
// the account itself), and refusals naming the entry for one that is not of that form, has a
// blank account, names a project and no person, names a namespace's default account or lists an
// account twice. It needs the cluster issuer and the exchange audience, whose tokens its pods
// present.
func TestLoadReadsTheExchangeAccounts(t *testing.T) {
	c, err := kubernetes.LoadConfig(getenv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Accounts != nil {
		t.Fatalf("Accounts = %v, want nil when BROKER_EXCHANGE_ACCOUNTS is unset", c.Accounts)
	}
	e := withExchange()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com\n" +
		" system:serviceaccount:example-ns:example-daemon=EXAMPLE=ada@example.com=legion-daemon:EXAMPLE=1 " +
		"system:serviceaccount:example-ns:example-public==ada@example.com " +
		"system:serviceaccount:example-ns:example-job== system:serviceaccount:example-ns:example-probe===example-probe-session"
	c, err = kubernetes.LoadConfig(getenv(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []kubernetes.AccountEntry{
		{Account: "system:serviceaccount:example-ns:example-worker", Project: "EXAMPLE", Person: "ada@example.com"},
		{Account: "system:serviceaccount:example-ns:example-daemon", Project: "EXAMPLE", Person: "ada@example.com", Session: "legion-daemon:EXAMPLE=1"},
		{Account: "system:serviceaccount:example-ns:example-public", Project: "", Person: "ada@example.com"},
		{Account: "system:serviceaccount:example-ns:example-job"},
		{Account: "system:serviceaccount:example-ns:example-probe", Session: "example-probe-session"},
	}
	if !slices.Equal(c.Accounts, want) {
		t.Fatalf("Accounts = %v, want %v", c.Accounts, want)
	}
	for _, bad := range []string{
		"system:serviceaccount:example-ns:example-worker", "system:serviceaccount:example-ns:example-worker=EXAMPLE",
		"system:serviceaccount:example-ns:example-worker=EXAMPLE=", "=EXAMPLE=ada@example.com",
		"example-worker=EXAMPLE=ada@example.com", "system:serviceaccount:example-ns:default=EXAMPLE=ada@example.com",
	} {
		e := withExchange()
		e["BROKER_EXCHANGE_ACCOUNTS"] = bad
		if _, err := kubernetes.LoadConfig(getenv(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_ACCOUNTS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_EXCHANGE_ACCOUNTS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	// An entry naming a project and no person is refused naming that rule, not only its form.
	e = withExchange()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=EXAMPLE="
	if _, err := kubernetes.LoadConfig(getenv(e)); err == nil || !strings.Contains(err.Error(), "names no person only when it names no project either") {
		t.Errorf("BROKER_EXCHANGE_ACCOUNTS naming a project and no person: err = %v, want a refusal naming the rule", err)
	}
	e = withExchange()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:x:y=A=ada@example.com system:serviceaccount:x:y=B=ada@example.com"
	if _, err := kubernetes.LoadConfig(getenv(e)); err == nil || !strings.Contains(err.Error(), "system:serviceaccount:x:y") {
		t.Errorf("BROKER_EXCHANGE_ACCOUNTS naming one account twice: err = %v, want a refusal naming the account", err)
	}
	e = withK8sIssuer()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com"
	if _, err := kubernetes.LoadConfig(getenv(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_K8S_AUDIENCE") {
		t.Errorf("BROKER_EXCHANGE_ACCOUNTS with no exchange audience: err = %v, want a refusal naming BROKER_EXCHANGE_K8S_AUDIENCE", err)
	}
	if _, err := kubernetes.LoadConfig(getenv(map[string]string{"BROKER_EXCHANGE_K8S_AUDIENCE": "dispatch-exchange"})); err == nil ||
		!strings.Contains(err.Error(), "BROKER_K8S_OIDC_ISSUER") {
		t.Errorf("BROKER_EXCHANGE_K8S_AUDIENCE with no cluster issuer: err = %v, want a refusal naming BROKER_K8S_OIDC_ISSUER", err)
	}
}

// TestLoadReadsTheExchangeReadAccounts pins BROKER_EXCHANGE_READ_ACCOUNTS: whitespace-separated
// <account>=<service>=<session> entries in order, none when unset, a session that may itself
// carry "=", and refusals naming the setting and the entry for one with fewer than three fields
// or a blank one, an account that is not a service account's subject, a namespace's default
// account, a service that is not a service's name or is the shared owner, and an account listed
// twice. An account BROKER_EXCHANGE_ACCOUNTS lists too is refused naming both settings, and the
// setting needs the cluster issuer and the exchange audience, whose tokens its pods present.
func TestLoadReadsTheExchangeReadAccounts(t *testing.T) {
	c, err := kubernetes.LoadConfig(getenv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Reads != nil {
		t.Fatalf("Reads = %v, want nil when BROKER_EXCHANGE_READ_ACCOUNTS is unset", c.Reads)
	}
	e := withExchange()
	e["BROKER_EXCHANGE_READ_ACCOUNTS"] = "system:serviceaccount:example-ns:example-reader=example-reader=example-reader-session\n" +
		" system:serviceaccount:example-ns:example-viewer=example-viewer=example-viewer=1 "
	c, err = kubernetes.LoadConfig(getenv(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []kubernetes.ReadEntry{
		{Account: "system:serviceaccount:example-ns:example-reader", Service: "example-reader", Session: "example-reader-session"},
		{Account: "system:serviceaccount:example-ns:example-viewer", Service: "example-viewer", Session: "example-viewer=1"},
	}
	if !slices.Equal(c.Reads, want) {
		t.Fatalf("Reads = %v, want %v", c.Reads, want)
	}
	for _, bad := range []string{
		"system:serviceaccount:example-ns:example-reader",
		"system:serviceaccount:example-ns:example-reader=example-reader",
		"=example-reader=example-reader-session",
		"system:serviceaccount:example-ns:example-reader==example-reader-session",
		"system:serviceaccount:example-ns:example-reader=example-reader=",
		"example-reader=example-reader=example-reader-session",
		"system:serviceaccount:example-ns:default=example-reader=example-reader-session",
		"system:serviceaccount:example-ns:example-reader=Example_Reader=example-reader-session",
		"system:serviceaccount:example-ns:example-reader=shared=example-reader-session",
	} {
		e := withExchange()
		e["BROKER_EXCHANGE_READ_ACCOUNTS"] = bad
		if _, err := kubernetes.LoadConfig(getenv(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_READ_ACCOUNTS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_EXCHANGE_READ_ACCOUNTS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	e = withExchange()
	const again = "system:serviceaccount:x:y=example-other=other-session"
	e["BROKER_EXCHANGE_READ_ACCOUNTS"] = "system:serviceaccount:x:y=example-reader=example-reader-session " + again
	if _, err := kubernetes.LoadConfig(getenv(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_READ_ACCOUNTS") || !strings.Contains(err.Error(), strconv.Quote(again)) {
		t.Errorf("BROKER_EXCHANGE_READ_ACCOUNTS naming one account twice: err = %v, want a refusal naming the setting and the entry", err)
	}
	e = withExchange()
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com"
	e["BROKER_EXCHANGE_READ_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=example-reader=example-reader-session"
	if _, err := kubernetes.LoadConfig(getenv(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_READ_ACCOUNTS") ||
		!strings.Contains(err.Error(), "BROKER_EXCHANGE_ACCOUNTS ") || !strings.Contains(err.Error(), "system:serviceaccount:example-ns:example-worker") {
		t.Errorf("an account both exchange settings list: err = %v, want a refusal naming both settings and the account", err)
	}
	e = withK8sIssuer()
	e["BROKER_EXCHANGE_READ_ACCOUNTS"] = "system:serviceaccount:example-ns:example-reader=example-reader=example-reader-session"
	if _, err := kubernetes.LoadConfig(getenv(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_READ_ACCOUNTS") || !strings.Contains(err.Error(), "BROKER_EXCHANGE_K8S_AUDIENCE") {
		t.Errorf("BROKER_EXCHANGE_READ_ACCOUNTS with no exchange audience: err = %v, want a refusal naming the setting and BROKER_EXCHANGE_K8S_AUDIENCE", err)
	}
}

// jobAccount is a pod's account that BROKER_EXCHANGE_JOB_ACCOUNTS lists; this repository is
// public, so it is a placeholder.
const jobAccount = "system:serviceaccount:example-ns:example-job"

// jobList is the job list exchange.ParseJobProjects reads from text.
func jobList(t *testing.T, text string) exchange.JobProjects {
	t.Helper()
	job, err := exchange.ParseJobProjects(text)
	if err != nil {
		t.Fatalf("exchange.ParseJobProjects(%q): %v", text, err)
	}
	return job
}

// TestLoadAppendsTheJobAccountsAfterTheAccounts pins where BROKER_EXCHANGE_JOB_ACCOUNTS lands:
// each entry, in order, joins Accounts after every BROKER_EXCHANGE_ACCOUNTS entry, holding its
// account, its projects as Job and the session it fixes, and no person and no project, so New
// numbers a job entry after the others. The setting needs the exchange audience, whose tokens its
// pods present.
func TestLoadAppendsTheJobAccountsAfterTheAccounts(t *testing.T) {
	const sweeper = "system:serviceaccount:example-ns:example-sweeper"
	e := withExchange()
	e["BROKER_EXCHANGE_JOB_ACCOUNTS"] = jobAccount + "=EXAMPLE=example-job:EXAMPLE=1\n " + sweeper + "=public,PRIVB,PRIVA"
	e["BROKER_EXCHANGE_ACCOUNTS"] = "system:serviceaccount:example-ns:example-worker=EXAMPLE=ada@example.com"
	c, err := kubernetes.LoadConfig(getenv(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []kubernetes.AccountEntry{
		{Account: "system:serviceaccount:example-ns:example-worker", Project: "EXAMPLE", Person: "ada@example.com"},
		{Account: jobAccount, Job: jobList(t, "EXAMPLE"), Session: "example-job:EXAMPLE=1"},
		{Account: sweeper, Job: jobList(t, "public,PRIVA,PRIVB")},
	}
	if !slices.Equal(c.Accounts, want) {
		t.Fatalf("Accounts = %+v, want %+v: the job entries after the account entries", c.Accounts, want)
	}
	e = withK8sIssuer()
	e["BROKER_EXCHANGE_JOB_ACCOUNTS"] = jobAccount + "=EXAMPLE"
	if _, err := kubernetes.LoadConfig(getenv(e)); err == nil || !strings.HasPrefix(err.Error(), "BROKER_EXCHANGE_JOB_ACCOUNTS needs BROKER_K8S_OIDC_ISSUER and BROKER_EXCHANGE_K8S_AUDIENCE") {
		t.Errorf("BROKER_EXCHANGE_JOB_ACCOUNTS with no exchange audience: err = %v, want a refusal naming the setting and BROKER_EXCHANGE_K8S_AUDIENCE", err)
	}
}

// TestLoadRefusesAnAccountTwoExchangeSettingsList: one account has one answer, so an account
// BROKER_EXCHANGE_ACCOUNTS, BROKER_EXCHANGE_READ_ACCOUNTS and BROKER_EXCHANGE_JOB_ACCOUNTS list
// more than once between them is refused, in config.Load's words: the later setting, the one
// that lists the account first, the account, and why a token of the later setting cannot share
// it. Each setting alone loads, so the refusal is the pair's.
func TestLoadRefusesAnAccountTwoExchangeSettingsList(t *testing.T) {
	for _, row := range []struct {
		name, first, firstValue, later, laterValue, want string
	}{
		{
			name:  "an account entry naming a person, and a job entry",
			first: "BROKER_EXCHANGE_ACCOUNTS", firstValue: jobAccount + "=EXAMPLE=ada@example.com",
			later: "BROKER_EXCHANGE_JOB_ACCOUNTS", laterValue: jobAccount + "=EXAMPLE",
			want: "BROKER_EXCHANGE_JOB_ACCOUNTS and BROKER_EXCHANGE_ACCOUNTS both name the service account " + jobAccount + "; one account mints one kind of token, and only a job's names the account as its service and carries projects, so name it in one of them",
		},
		{
			name:  "an account entry naming neither a person nor a project, and a job entry",
			first: "BROKER_EXCHANGE_ACCOUNTS", firstValue: jobAccount + "==",
			later: "BROKER_EXCHANGE_JOB_ACCOUNTS", laterValue: jobAccount + "=public",
			want: "BROKER_EXCHANGE_JOB_ACCOUNTS and BROKER_EXCHANGE_ACCOUNTS both name the service account " + jobAccount + "; one account mints one kind of token, and only a job's names the account as its service and carries projects, so name it in one of them",
		},
		{
			name:  "a read entry, and a job entry",
			first: "BROKER_EXCHANGE_READ_ACCOUNTS", firstValue: jobAccount + "=example-reader=example-reader-session",
			later: "BROKER_EXCHANGE_JOB_ACCOUNTS", laterValue: jobAccount + "=public",
			want: "BROKER_EXCHANGE_JOB_ACCOUNTS and BROKER_EXCHANGE_READ_ACCOUNTS both name the service account " + jobAccount + "; one account mints one kind of token, and only a job's names the account as its service and carries projects, so name it in one of them",
		},
		{
			name:  "an account entry, and a read entry",
			first: "BROKER_EXCHANGE_ACCOUNTS", firstValue: jobAccount + "==",
			later: "BROKER_EXCHANGE_READ_ACCOUNTS", laterValue: jobAccount + "=example-reader=example-reader-session",
			want: "BROKER_EXCHANGE_READ_ACCOUNTS and BROKER_EXCHANGE_ACCOUNTS both name the service account " + jobAccount + "; one account mints one kind of token, a read-only one or one that writes as its person or as the account, so name it in one of them",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, alone := range [][2]string{{row.first, row.firstValue}, {row.later, row.laterValue}} {
				e := withExchange()
				e[alone[0]] = alone[1]
				if _, err := kubernetes.LoadConfig(getenv(e)); err != nil {
					t.Fatalf("LoadConfig(%s=%q) alone: %v; want it to load, so that the refusal below is the pair's", alone[0], alone[1], err)
				}
			}
			e := withExchange()
			e[row.first], e[row.later] = row.firstValue, row.laterValue
			if _, err := kubernetes.LoadConfig(getenv(e)); err == nil || err.Error() != row.want {
				t.Fatalf("LoadConfig(%s=%q, %s=%q) = %v, want %q", row.first, row.firstValue, row.later, row.laterValue, err, row.want)
			}
		})
	}
}

// TestMentionsSaysWhatEachEntryGivesTheAccount pins what config's BROKER_POD_OPERATORS check
// reads of this provider: an account entry's person, or none for an entry naming neither a person
// nor a project, a job entry's list under BROKER_EXCHANGE_JOB_ACCOUNTS, and a read entry's
// service, and nothing for an account of another kind.
func TestMentionsSaysWhatEachEntryGivesTheAccount(t *testing.T) {
	const person, personless, reader = "system:serviceaccount:example-ns:example-worker", "system:serviceaccount:example-ns:example-probe", "system:serviceaccount:example-ns:example-reader"
	e := withExchange()
	e["BROKER_EXCHANGE_ACCOUNTS"] = person + "=EXAMPLE=ada@example.com " + personless + "=="
	e["BROKER_EXCHANGE_READ_ACCOUNTS"] = reader + "=example-reader=example-reader-session"
	e["BROKER_EXCHANGE_JOB_ACCOUNTS"] = jobAccount + "=public,PRIVA"
	c, err := kubernetes.LoadConfig(getenv(e))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		account string
		want    identity.Mention
	}{
		{person, identity.Mention{Variable: "BROKER_EXCHANGE_ACCOUNTS", Person: "ada@example.com"}},
		{personless, identity.Mention{Variable: "BROKER_EXCHANGE_ACCOUNTS"}},
		{reader, identity.Mention{Variable: "BROKER_EXCHANGE_READ_ACCOUNTS", Service: "example-reader", ReadOnly: true}},
		{jobAccount, identity.Mention{Variable: "BROKER_EXCHANGE_JOB_ACCOUNTS", Job: jobList(t, "public,PRIVA")}},
	} {
		got := c.Mentions(identity.Account{Kind: kubernetes.Kind, Address: row.account})
		if want := []identity.Mention{row.want}; !slices.Equal(got, want) {
			t.Errorf("Mentions(%s) = %+v, want %+v", row.account, got, want)
		}
	}
	if got := c.Mentions(identity.Account{Kind: "google", Address: jobAccount}); got != nil {
		t.Errorf("Mentions of a google account = %+v, want none", got)
	}
}
