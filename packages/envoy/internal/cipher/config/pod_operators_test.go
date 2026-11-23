// packages/envoy/internal/cipher/config/pod_operators_test.go
package config

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The accounts BROKER_POD_OPERATORS maps in these tests, and the login service each one logs in
// under. This repository is public, so each name is a placeholder.
const (
	adaAccount       = "system:serviceaccount:people:ada"
	adaSecondAccount = "system:serviceaccount:people:ada-batch"
	graceAccount     = "system:serviceaccount:people:grace"
	adaLoginService  = "person-ada"
	// adaWorker is a service's worker account that a BROKER_SESSION_ACCOUNT_PAIRS row pairs with
	// ada's account.
	adaWorker = "system:serviceaccount:people:ada-worker"
)

// adaPodOperator is the one BROKER_POD_OPERATORS entry the refusal tests set: ada's own account,
// mapped to ada.
const adaPodOperator = "ada@example.com=" + adaAccount

// TestPodOperatorsRefusesAnEntryItCannotRead: each entry that is not
// <email>=system:serviceaccount:<namespace>:<name>, with a lower-case email and a dedicated
// account given once, refuses the start. The refusal names the setting and the entry.
func TestPodOperatorsRefusesAnEntryItCannotRead(t *testing.T) {
	// The environment each row starts from: ada's account logs in under its own login service,
	// so only the row's entry can make Load refuse.
	base := func() map[string]string {
		e := withK8sIssuer()
		e["BROKER_SERVICE_LOGINS"] = adaLoginService + "=" + adaAccount
		return e
	}
	// The precondition of every row: the same environment with a valid entry loads.
	valid := base()
	valid["BROKER_POD_OPERATORS"] = adaPodOperator
	if _, err := Load(env(valid)); err != nil {
		t.Fatalf("BROKER_POD_OPERATORS = %q with BROKER_SERVICE_LOGINS naming %s: %v, want it to load, so that each refusal below is the entry's", adaPodOperator, adaAccount, err)
	}
	// The words of each refusal that no other refusal of the setting says.
	const (
		notAnEntry     = "must be <email>=system:serviceaccount:<namespace>:<name> entries"
		notAPerson     = "must name each person by their email in lower case"
		notAnAccount   = "must name system:serviceaccount:<namespace>:<name> accounts"
		defaultAccount = "names a namespace's default service account"
		twice          = "twice"
	)
	for _, tc := range []struct {
		name, value string
		// names is what the refusal must say besides the setting's name: the entry, and the
		// words of the one refusal the row is for.
		names []string
	}{
		{name: "an entry with no =", value: "ada@example.com", names: []string{strconv.Quote("ada@example.com"), notAnEntry}},
		{name: "the account first", value: adaAccount + "=ada@example.com", names: []string{strconv.Quote(adaAccount + "=ada@example.com"), notAPerson}},
		{name: "an upper-case person", value: "Ada@example.com=" + adaAccount, names: []string{strconv.Quote("Ada@example.com=" + adaAccount), notAPerson}},
		{name: "a person that is no email", value: "ada=" + adaAccount, names: []string{strconv.Quote("ada=" + adaAccount), notAPerson}},
		{name: "an account that is no ServiceAccount subject", value: "ada@example.com=system:serviceaccount:People:ada", names: []string{strconv.Quote("ada@example.com=system:serviceaccount:People:ada"), notAnAccount}},
		{name: "a namespace's default account", value: "ada@example.com=system:serviceaccount:people:default", names: []string{strconv.Quote("ada@example.com=system:serviceaccount:people:default"), defaultAccount}},
		{
			name:  "an account given twice",
			value: adaPodOperator + " grace@example.com=" + adaAccount,
			names: []string{adaAccount, strconv.Quote("grace@example.com=" + adaAccount), twice},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := base()
			e["BROKER_POD_OPERATORS"] = tc.value
			cfg, err := Load(env(e))
			if err == nil {
				t.Fatalf("BROKER_POD_OPERATORS = %q loaded, PodOperators = %v; want a refusal naming the entry", tc.value, cfg.PodOperators)
			}
			for _, want := range append([]string{"BROKER_POD_OPERATORS"}, tc.names...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("BROKER_POD_OPERATORS = %q: refusal %q does not name %s", tc.value, err, want)
				}
			}
		})
	}
}

// mappedAccountRefusalCase is one row of mappedAccountRefusalCases.
type mappedAccountRefusalCase struct {
	name string
	env  func() map[string]string
	// reachable says what the environment holds with BROKER_POD_OPERATORS unset, and
	// whether it holds.
	reachable func(cfg Config) (string, bool)
	// names is what the refusal must say besides the account and BROKER_POD_OPERATORS: the
	// other setting, in the words of the one refusal the row is for.
	names []string
}

// mappedAccountRefusalCases are the rows of
// TestPodOperatorsRefusesAMappedAccountAnotherSettingGivesARole, one or more for each setting
// mappedAccountRefusal checks. TestEveryBrokerSettingLoadReadsIsMarked reads their names, beside
// which its table marks each setting that gives a Kubernetes account a role, and loads their
// environments.
func mappedAccountRefusalCases(t *testing.T) []mappedAccountRefusalCase {
	natsUser, natsIssuer, natsCurve, _ := natsSeeds(t)
	return []mappedAccountRefusalCase{
		{
			name: "BROKER_SERVICE_LOGINS does not name it",
			env: func() map[string]string {
				e := withK8sIssuer()
				e["BROKER_SERVICE_LOGINS"] = "person-grace=" + graceAccount
				return e
			},
			reachable: func(cfg Config) (string, bool) {
				_, listed := cfg.ServiceLogins[adaAccount]
				return "BROKER_SERVICE_LOGINS is set and does not name " + adaAccount, len(cfg.ServiceLogins) > 0 && !listed
			},
			names: []string{"BROKER_SERVICE_LOGINS does not name it"},
		},
		{
			name: "a BROKER_SERVICES entry names its login service",
			env: func() map[string]string {
				e := withK8sIssuer()
				e["BROKER_SERVICE_LOGINS"] = adaLoginService + "=" + adaAccount
				e["BROKER_SERVICES"] = adaLoginService + "=system:serviceaccount:people:ada-worker"
				return e
			},
			reachable: func(cfg Config) (string, bool) {
				_, binds := cfg.ServicesByAccount[adaAccount]
				return "BROKER_SERVICES names the login service " + adaLoginService + " of " + adaAccount + " and does not bind the account itself",
					cfg.ServiceLogins[adaAccount] == adaLoginService && slices.Contains(slices.Collect(maps.Values(cfg.ServicesByAccount)), adaLoginService) && !binds
			},
			names: []string{"BROKER_SERVICES names its login service " + strconv.Quote(adaLoginService)},
		},
		{
			name: "a BROKER_SERVICES entry binds it",
			env: func() map[string]string {
				e := withK8sIssuer()
				e["BROKER_SERVICE_LOGINS"] = adaLoginService + "=" + adaAccount
				e["BROKER_SERVICES"] = "legion-daemon=" + adaAccount
				return e
			},
			reachable: func(cfg Config) (string, bool) {
				return "BROKER_SERVICES binds " + adaAccount + " to legion-daemon and names no service " + adaLoginService,
					cfg.ServiceLogins[adaAccount] == adaLoginService && cfg.ServicesByAccount[adaAccount] == "legion-daemon" &&
						!slices.Contains(slices.Collect(maps.Values(cfg.ServicesByAccount)), adaLoginService)
			},
			names: []string{"BROKER_SERVICES binds it to the service " + strconv.Quote("legion-daemon")},
		},
		{
			name: "a BROKER_VM_SERVICES entry names its login service",
			env: func() map[string]string {
				e := validVMEnv()
				maps.Copy(e, withK8sIssuer())
				e["BROKER_SERVICE_LOGINS"] = "example-service=" + adaAccount
				return e
			},
			reachable: func(cfg Config) (string, bool) {
				return "BROKER_VM_SERVICES names example-service, the login service of " + adaAccount + ", and BROKER_SERVICES is unset",
					cfg.ServiceLogins[adaAccount] == "example-service" && cfg.VMServicesByAccount[exampleVMServiceAccount] == "example-service" && len(cfg.ServicesByAccount) == 0
			},
			names: []string{"BROKER_VM_SERVICES names its login service " + strconv.Quote("example-service")},
		},
		{
			name: "a BROKER_EXCHANGE_ACCOUNTS entry maps it to another person",
			env: func() map[string]string {
				e := withK8sIssuer()
				e["BROKER_SERVICE_LOGINS"] = adaLoginService + "=" + adaAccount
				e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch"
				e["BROKER_EXCHANGE_ACCOUNTS"] = adaAccount + "=EXAMPLE=grace@example.com"
				return e
			},
			reachable: func(cfg Config) (string, bool) {
				return "BROKER_EXCHANGE_ACCOUNTS maps " + adaAccount + " to grace@example.com",
					len(cfg.ExchangeAccounts) == 1 && cfg.ExchangeAccounts[0].Account == adaAccount && cfg.ExchangeAccounts[0].Person == "grace@example.com"
			},
			names: []string{"BROKER_EXCHANGE_ACCOUNTS maps it to grace@example.com"},
		},
		{
			name: "a BROKER_NATS_SERVICES entry names it",
			env: func() map[string]string {
				e := withK8sIssuer()
				e["BROKER_SERVICE_LOGINS"] = adaLoginService + "=" + adaAccount
				e["BROKER_NATS_URLS"] = "nats://hub-a:4222"
				e["BROKER_NATS_SEED_FILE"] = natsUser
				e["BROKER_NATS_ISSUER_SEED_FILE"] = natsIssuer
				e["BROKER_NATS_XKEY_SEED_FILE"] = natsCurve
				e["BROKER_NATS_SERVICES"] = "daemon/-/*=" + adaAccount
				return e
			},
			reachable: func(cfg Config) (string, bool) {
				return "the NATS auth service runs, and BROKER_NATS_SERVICES names " + adaAccount,
					cfg.NATS.Enabled() && entryTexts(cfg.NATS.Services) == "daemon/-/*="+adaAccount && cfg.ServiceLogins[adaAccount] == adaLoginService
			},
			names: []string{"the BROKER_NATS_SERVICES entry " + strconv.Quote("daemon/-/*="+adaAccount) + " names it"},
		},
		{
			name: "a BROKER_API_ACCOUNTS entry names it",
			env: func() map[string]string {
				e := withK8sIssuer()
				e["BROKER_SERVICE_LOGINS"] = adaLoginService + "=" + adaAccount
				e["BROKER_API_ACCOUNTS"] = "dispatch=" + adaAccount
				return e
			},
			reachable: func(cfg Config) (string, bool) {
				caller, calls := cfg.APIAccounts[adaAccount]
				return "BROKER_API_ACCOUNTS names " + adaAccount + " as the caller dispatch",
					calls && caller.Name == "dispatch" && cfg.ServiceLogins[adaAccount] == adaLoginService
			},
			names: []string{"BROKER_API_ACCOUNTS names it as the caller " + strconv.Quote("dispatch")},
		},
		{
			name: "a BROKER_POD_REGISTRARS entry names it as its registrar",
			env: func() map[string]string {
				e := withK8sIssuer()
				e["BROKER_SERVICE_LOGINS"] = adaLoginService + "=" + adaAccount
				e["BROKER_POD_REGISTRARS"] = adaAccount + "=" + adaWorker
				return e
			},
			reachable: func(cfg Config) (string, bool) {
				return "BROKER_POD_REGISTRARS pairs " + adaAccount + ", as the registrar, with " + adaWorker,
					len(cfg.PodRegistrars) == 1 && cfg.PodRegistrars.Lists(adaAccount, adaWorker) && cfg.ServiceLogins[adaAccount] == adaLoginService
			},
			names: []string{"the BROKER_POD_REGISTRARS entry " + strconv.Quote(adaAccount+"="+adaWorker) + " names it as a registrar"},
		},
		{
			name: "a BROKER_EXCHANGE_READ_ACCOUNTS entry names it",
			env: func() map[string]string {
				e := withK8sIssuer()
				e["BROKER_SERVICE_LOGINS"] = adaLoginService + "=" + adaAccount
				e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch"
				e["BROKER_EXCHANGE_READ_ACCOUNTS"] = adaAccount + "=example-reader=example-read-session"
				return e
			},
			reachable: func(cfg Config) (string, bool) {
				return "BROKER_EXCHANGE_READ_ACCOUNTS names " + adaAccount + " as a reader of example-reader",
					len(cfg.ExchangeReadAccounts) == 1 && cfg.ExchangeReadAccounts[0].Account == adaAccount && cfg.ExchangeReadAccounts[0].Service == "example-reader" &&
						cfg.ServiceLogins[adaAccount] == adaLoginService
			},
			names: []string{"BROKER_EXCHANGE_READ_ACCOUNTS names it as a reader of the service " + strconv.Quote("example-reader")},
		},
		{
			name: "a BROKER_SESSION_ACCOUNT_PAIRS pair names it as the first account",
			env: func() map[string]string {
				e := withK8sIssuer()
				e["BROKER_SERVICE_LOGINS"] = adaLoginService + "=" + adaAccount
				e["BROKER_SESSION_ACCOUNT_PAIRS"] = adaAccount + "=" + adaWorker
				return e
			},
			reachable: func(cfg Config) (string, bool) {
				return "BROKER_SESSION_ACCOUNT_PAIRS pairs " + adaAccount + " with " + adaWorker,
					len(cfg.SessionAccountPairs) == 1 && cfg.SessionAccountPairs[0] == [2]string{adaAccount, adaWorker} && cfg.ServiceLogins[adaAccount] == adaLoginService
			},
			names: []string{"the BROKER_SESSION_ACCOUNT_PAIRS pair " + strconv.Quote(adaAccount+"="+adaWorker) + " names it"},
		},
		{
			name: "a BROKER_SESSION_ACCOUNT_PAIRS pair names it as the second account",
			env: func() map[string]string {
				e := withK8sIssuer()
				e["BROKER_SERVICE_LOGINS"] = adaLoginService + "=" + adaAccount
				e["BROKER_SESSION_ACCOUNT_PAIRS"] = adaWorker + "=" + adaAccount
				return e
			},
			reachable: func(cfg Config) (string, bool) {
				return "BROKER_SESSION_ACCOUNT_PAIRS pairs " + adaWorker + " with " + adaAccount,
					len(cfg.SessionAccountPairs) == 1 && cfg.SessionAccountPairs[0] == [2]string{adaWorker, adaAccount} && cfg.ServiceLogins[adaAccount] == adaLoginService
			},
			names: []string{"the BROKER_SESSION_ACCOUNT_PAIRS pair " + strconv.Quote(adaWorker+"="+adaAccount) + " names it"},
		},
	}
}

// TestPodOperatorsRefusesAMappedAccountAnotherSettingGivesARole: Load refuses a mapped account in
// each case of mappedAccountRefusalCases, and the refusal names the account and both settings.
// Each row first loads its environment with BROKER_POD_OPERATORS unset and asserts the state that
// makes the refusal reachable, so the refusal is the mapping's and not a fault of the other
// settings.
func TestPodOperatorsRefusesAMappedAccountAnotherSettingGivesARole(t *testing.T) {
	for _, tc := range mappedAccountRefusalCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			unmapped, err := Load(env(tc.env()))
			if err != nil {
				t.Fatalf("with BROKER_POD_OPERATORS unset, Load: %v; want it to load, so that the refusal of %s is the mapping's", err, adaAccount)
			}
			if state, holds := tc.reachable(unmapped); !holds {
				t.Fatalf("with BROKER_POD_OPERATORS unset, want %s; ServiceLogins = %v, ServicesByAccount = %v, VMServicesByAccount = %v, ExchangeAccounts = %+v, ExchangeReadAccounts = %+v, APIAccounts = %v, SessionAccountPairs = %v, PodRegistrars = %v, NATS = %+v",
					state, unmapped.ServiceLogins, unmapped.ServicesByAccount, unmapped.VMServicesByAccount, unmapped.ExchangeAccounts, unmapped.ExchangeReadAccounts, unmapped.APIAccounts, unmapped.SessionAccountPairs, unmapped.PodRegistrars, unmapped.NATS)
			}
			e := tc.env()
			e["BROKER_POD_OPERATORS"] = adaPodOperator
			cfg, err := Load(env(e))
			if err == nil {
				t.Fatalf("BROKER_POD_OPERATORS mapping %s to ada@example.com loaded, PodOperators = %v; want a refusal naming %s, since %s", adaAccount, cfg.PodOperators, adaAccount, tc.name)
			}
			for _, want := range append([]string{adaAccount, "BROKER_POD_OPERATORS"}, tc.names...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("BROKER_POD_OPERATORS mapping %s: refusal %q does not name %s", adaAccount, err, want)
				}
			}
		})
	}
}

// TestPodOperatorsLoadsMappedAccounts: valid entries, separated by any white space, load into
// PodOperators keyed by account, each answering its person in record.CanonicalLogin's form. A
// person may have two accounts. A BROKER_EXCHANGE_ACCOUNTS entry that maps a mapped account to the
// same person, in other letter case, is no refusal. Nor is a BROKER_SERVICES entry that binds
// another account to another service, nor a BROKER_NATS_SERVICES entry that names another
// account, nor a BROKER_API_ACCOUNTS caller of another Kubernetes account or of a Google account,
// nor a BROKER_EXCHANGE_READ_ACCOUNTS reader of another account, nor a BROKER_POD_REGISTRARS entry
// that names a mapped account as the pod account a daemon registers, nor a
// BROKER_SESSION_ACCOUNT_PAIRS pair of two accounts the setting does not map. No entry gives a nil
// map.
func TestPodOperatorsLoadsMappedAccounts(t *testing.T) {
	natsUser, natsIssuer, natsCurve, _ := natsSeeds(t)
	e := withK8sIssuer()
	e["BROKER_SERVICE_LOGINS"] = adaLoginService + "=" + adaAccount + " person-ada-batch=" + adaSecondAccount + " person-grace=" + graceAccount
	e["BROKER_SERVICES"] = "legion-daemon=system:serviceaccount:legion:legion-worker"
	e["BROKER_NATS_URLS"] = "nats://hub-a:4222"
	e["BROKER_NATS_SEED_FILE"] = natsUser
	e["BROKER_NATS_ISSUER_SEED_FILE"] = natsIssuer
	e["BROKER_NATS_XKEY_SEED_FILE"] = natsCurve
	e["BROKER_NATS_SERVICES"] = "daemon/-/*=system:serviceaccount:legion-system:legion-daemon"
	e["BROKER_API_ACCOUNTS"] = "dispatch=system:serviceaccount:example-ns:example-dispatch listener=google:" + exampleListenerAccount
	e["BROKER_GOOGLE_OIDC_AUDIENCE"] = "example-broker"
	e["BROKER_SESSION_ACCOUNT_PAIRS"] = "system:serviceaccount:legion:legion-worker=system:serviceaccount:legion:example-worker"
	e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch"
	e["BROKER_EXCHANGE_ACCOUNTS"] = adaAccount + "=EXAMPLE=Ada@Example.com"
	e["BROKER_EXCHANGE_READ_ACCOUNTS"] = "system:serviceaccount:example-ns:example-reader=example-reader=example-read-session"
	e["BROKER_POD_REGISTRARS"] = "system:serviceaccount:legion-system:legion-daemon=" + adaAccount
	e["BROKER_POD_OPERATORS"] = " " + adaPodOperator + "\n\tada@example.com=" + adaSecondAccount + " grace@example.com=" + graceAccount + " "
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{adaAccount: "ada@example.com", adaSecondAccount: "ada@example.com", graceAccount: "grace@example.com"}
	if !maps.Equal(cfg.PodOperators, want) {
		t.Fatalf("PodOperators = %v, want %v", cfg.PodOperators, want)
	}
	// The other settings loaded as written, so each is beside the mapping and refuses nothing.
	if len(cfg.APIAccounts) != 1 || len(cfg.GoogleAPIAccounts) != 1 || len(cfg.SessionAccountPairs) != 1 || len(cfg.ExchangeReadAccounts) != 1 || !cfg.NATS.Enabled() ||
		len(cfg.PodRegistrars) != 1 || !cfg.PodRegistrars.Lists("system:serviceaccount:legion-system:legion-daemon", adaAccount) {
		t.Fatalf("APIAccounts = %v, GoogleAPIAccounts = %v, SessionAccountPairs = %v, ExchangeReadAccounts = %+v, PodRegistrars = %v, NATS = %+v; want one Kubernetes caller, one Google caller, one pair, one reader, one registrar pairing a daemon with %s as its pod account, and the NATS auth service",
			cfg.APIAccounts, cfg.GoogleAPIAccounts, cfg.SessionAccountPairs, cfg.ExchangeReadAccounts, cfg.PodRegistrars, cfg.NATS, adaAccount)
	}
	if people, err := parsePodOperators("BROKER_POD_OPERATORS", " \n\t "); err != nil || people != nil {
		t.Fatalf("parsePodOperators of white space alone = %v, %v; want nil and no error", people, err)
	}
}

// TestPodOperatorsUnsetMapsNoDaemonToAPerson: each Legion daemon's account logs in under
// BROKER_SERVICE_LOGINS, and BROKER_EXCHANGE_ACCOUNTS maps it to a person for Dispatch. With
// BROKER_POD_OPERATORS unset, Load maps no account to a person.
func TestPodOperatorsUnsetMapsNoDaemonToAPerson(t *testing.T) {
	const (
		alphaDaemon = "system:serviceaccount:legion-system:legion-daemon-alpha"
		betaDaemon  = "system:serviceaccount:legion-system:legion-daemon-beta"
	)
	e := withK8sIssuer()
	e["BROKER_SERVICE_LOGINS"] = "legion-daemon=" + alphaDaemon + " legion-daemon=" + betaDaemon
	e["BROKER_EXCHANGE_K8S_AUDIENCE"] = "dispatch"
	e["BROKER_EXCHANGE_ACCOUNTS"] = alphaDaemon + "=ALPHA=ada@example.com " + betaDaemon + "=BETA=ada@example.com"
	cfg, err := Load(env(e))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{alphaDaemon: "legion-daemon", betaDaemon: "legion-daemon"}; !maps.Equal(cfg.ServiceLogins, want) {
		t.Fatalf("ServiceLogins = %v, want %v", cfg.ServiceLogins, want)
	}
	if len(cfg.ExchangeAccounts) != 2 || cfg.ExchangeAccounts[0].Person != "ada@example.com" || cfg.ExchangeAccounts[1].Person != "ada@example.com" {
		t.Fatalf("ExchangeAccounts = %+v, want both daemons mapped to ada@example.com", cfg.ExchangeAccounts)
	}
	if cfg.PodOperators != nil {
		t.Fatalf("PodOperators = %v, want nil: BROKER_POD_OPERATORS is unset, so no daemon acts for ada@example.com at the broker", cfg.PodOperators)
	}
}
