package kubernetes

import (
	"fmt"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/oidc"
)

// AccountForm is how a Kubernetes account is written wherever the broker's settings name one.
const AccountForm = "system:serviceaccount:<namespace>:<name>"

// defaultServiceAccount is the account name Kubernetes gives every namespace and runs every pod
// whose spec names no account as.
const defaultServiceAccount = "default"

// LoadConfig reads this provider's own variables from getenv, in the order config.Load read them:
// BROKER_K8S_OIDC_ISSUER and BROKER_K8S_OIDC_AUDIENCE (each needing the other),
// BROKER_EXCHANGE_K8S_AUDIENCE, BROKER_EXCHANGE_ACCOUNTS, BROKER_EXCHANGE_READ_ACCOUNTS,
// BROKER_EXCHANGE_JOB_ACCOUNTS, BROKER_NATS_K8S_AUDIENCE and BROKER_POD_REGISTRATION_AUDIENCE.
// The job entries join Accounts after BROKER_EXCHANGE_ACCOUNTS' own, so New numbers a job entry
// after every other account entry. Every refusal is the one config.Load answered, word for word.
func LoadConfig(getenv func(string) string) (Config, error) {
	issuer, audience, err := oidc.ConfigFromEnv(getenv, "BROKER_K8S_OIDC_ISSUER", "BROKER_K8S_OIDC_AUDIENCE")
	if err != nil {
		return Config{}, err
	}
	c := Config{
		Issuer: issuer, Audience: audience,
		ExchangeAudience: strings.TrimSpace(getenv("BROKER_EXCHANGE_K8S_AUDIENCE")),
		NATSAudience:     strings.TrimSpace(getenv("BROKER_NATS_K8S_AUDIENCE")),
	}
	if c.ExchangeAudience != "" && c.Issuer == "" {
		return Config{}, fmt.Errorf("BROKER_EXCHANGE_K8S_AUDIENCE needs BROKER_K8S_OIDC_ISSUER, the cluster whose pods carry it")
	}
	if c.Accounts, err = parseAccounts("BROKER_EXCHANGE_ACCOUNTS", getenv("BROKER_EXCHANGE_ACCOUNTS")); err != nil {
		return Config{}, err
	}
	if len(c.Accounts) > 0 && c.ExchangeAudience == "" {
		return Config{}, fmt.Errorf("BROKER_EXCHANGE_ACCOUNTS needs BROKER_K8S_OIDC_ISSUER and BROKER_EXCHANGE_K8S_AUDIENCE, whose tokens its pods present")
	}
	if c.Reads, err = parseReadAccounts("BROKER_EXCHANGE_READ_ACCOUNTS", getenv("BROKER_EXCHANGE_READ_ACCOUNTS")); err != nil {
		return Config{}, err
	}
	if len(c.Reads) > 0 && c.ExchangeAudience == "" {
		return Config{}, fmt.Errorf("BROKER_EXCHANGE_READ_ACCOUNTS needs BROKER_K8S_OIDC_ISSUER and BROKER_EXCHANGE_K8S_AUDIENCE, whose tokens its pods present")
	}
	jobs, err := parseJobAccounts("BROKER_EXCHANGE_JOB_ACCOUNTS", getenv("BROKER_EXCHANGE_JOB_ACCOUNTS"))
	if err != nil {
		return Config{}, err
	}
	if len(jobs) > 0 && c.ExchangeAudience == "" {
		return Config{}, fmt.Errorf("BROKER_EXCHANGE_JOB_ACCOUNTS needs BROKER_K8S_OIDC_ISSUER and BROKER_EXCHANGE_K8S_AUDIENCE, whose tokens its pods present")
	}
	if err := listingRefusal(c.Accounts, c.Reads, jobs); err != nil {
		return Config{}, err
	}
	c.Accounts = append(c.Accounts, jobs...)
	if c.NATSAudience != "" && c.Issuer == "" {
		return Config{}, fmt.Errorf("BROKER_NATS_K8S_AUDIENCE needs BROKER_K8S_OIDC_ISSUER, whose projected tokens a service presents to the NATS auth service")
	}
	c.PodRegistrationAudience = strings.TrimSpace(getenv("BROKER_POD_REGISTRATION_AUDIENCE"))
	if c.PodRegistrationAudience != "" {
		if c.Issuer == "" {
			return Config{}, fmt.Errorf("BROKER_POD_REGISTRATION_AUDIENCE needs BROKER_K8S_OIDC_ISSUER, the cluster whose pods' tokens carry it")
		}
		// Each other audience a token of the cluster's issuer is accepted at, by the route that
		// accepts it: equal to one of them, a token minted for registering a pod would be
		// spendable there too, and one minted for there would register a pod.
		for _, other := range []struct{ setting, audience string }{
			{"BROKER_K8S_OIDC_AUDIENCE", c.Audience},
			{"BROKER_EXCHANGE_K8S_AUDIENCE", c.ExchangeAudience},
			{"BROKER_NATS_K8S_AUDIENCE", c.NATSAudience},
		} {
			if other.audience == c.PodRegistrationAudience {
				return Config{}, fmt.Errorf("BROKER_POD_REGISTRATION_AUDIENCE is %q, which %s is too: a token of the registration audience is accepted at POST /v1/oauth/exchange/pod-sessions alone, so give it an audience no other setting names", c.PodRegistrationAudience, other.setting)
			}
		}
	}
	return c, nil
}

// Serves is the sentence a Kubernetes account's site needs when this provider verifies no caller
// of use: an account of this kind could never authenticate without the issuer whose tokens it
// presents, so the start is refused where the broker's settings name one. It answers nil where
// the setting that use needs is set.
func (c Config) Serves(use identity.Use, site identity.Site) error {
	switch {
	case use == identity.UseAPI && c.Issuer == "" && site.Edge:
		return fmt.Errorf("%s entry %q names a Kubernetes service account as an edge listener, which needs BROKER_K8S_OIDC_ISSUER and BROKER_K8S_OIDC_AUDIENCE, whose tokens it presents", site.Setting, site.Entry)
	case use == identity.UseAPI && c.Issuer == "":
		return fmt.Errorf("%s names a Kubernetes service account, which needs BROKER_K8S_OIDC_ISSUER and BROKER_K8S_OIDC_AUDIENCE, whose tokens its callers present", site.Setting)
	case use == identity.UseNATS && c.NATSAudience == "":
		return fmt.Errorf("%s lists %s, a Kubernetes account, and BROKER_NATS_K8S_AUDIENCE is unset, so its token could not be verified", site.Setting, site.Entry)
	}
	return nil
}

// PodsServed is the sentence a setting about pods needs when this provider trusts no cluster: a
// pod's token could be verified against nothing.
func (c Config) PodsServed(setting string) error {
	if c.Issuer != "" {
		return nil
	}
	return fmt.Errorf("%s needs BROKER_K8S_OIDC_ISSUER and BROKER_K8S_OIDC_AUDIENCE, whose tokens its accounts log in with", setting)
}

// Mentions is every place this provider's settings name account: its exchange entry, with the
// person it gives the account or none, its job entry, with the job list it gives it, and its read
// entry, with its service.
func (c Config) Mentions(account identity.Account) []identity.Mention {
	if account.Kind != Kind {
		return nil
	}
	var mentions []identity.Mention
	for _, entry := range c.Accounts {
		if entry.Account != account.Address {
			continue
		}
		if entry.Job.Listed() {
			mentions = append(mentions, identity.Mention{Variable: "BROKER_EXCHANGE_JOB_ACCOUNTS", Job: entry.Job})
			continue
		}
		mentions = append(mentions, identity.Mention{Variable: "BROKER_EXCHANGE_ACCOUNTS", Person: entry.Person})
	}
	for _, entry := range c.Reads {
		if entry.Account == account.Address {
			mentions = append(mentions, identity.Mention{Variable: "BROKER_EXCHANGE_READ_ACCOUNTS", Service: entry.Service, ReadOnly: true})
		}
	}
	return mentions
}

// Services is every service this provider's entries name: a read entry's alone, since an account
// entry names a person or acts as the account itself, and a job entry names the account.
func (c Config) Services() []string {
	var services []string
	for _, entry := range c.Reads {
		services = append(services, entry.Service)
	}
	return services
}

// ParseAccount reads account as a Kubernetes service account's subject, answering false for text
// of another form, which another provider may claim. It refuses, naming setting and the entry
// text it came from, a namespace's default account: every pod in that namespace whose spec names
// no account runs as it, so its token proves nothing.
func ParseAccount(setting, text, account string) (identity.Account, bool, error) {
	if !oauthwire.AccountSubject.MatchString(account) {
		return identity.Account{}, false, nil
	}
	if isDefaultServiceAccount(account) {
		return identity.Account{}, true, DefaultAccountRefusal(setting, text)
	}
	return identity.Account{Kind: Kind, Address: account}, true, nil
}

// isDefaultServiceAccount reports whether subject is a namespace's default account, the one
// Kubernetes gives every namespace and runs every pod whose spec names no account as. No setting
// takes one: a token for it says only that some pod of that namespace is asking.
func isDefaultServiceAccount(subject string) bool {
	_, account, _ := strings.Cut(strings.TrimPrefix(subject, "system:serviceaccount:"), ":")
	return account == defaultServiceAccount
}

// DefaultAccountRefusal is the refusal of setting's entry text for naming a namespace's default
// account, whose token any pod of that namespace whose spec names no account can get for the
// broker's audience. Every setting that names a Kubernetes account shares it, so each refuses
// such an account in the same words.
func DefaultAccountRefusal(setting, text string) error {
	return fmt.Errorf("%s names a namespace's default service account, got %q: every pod in that namespace whose spec names no account runs as it and can get a token for the broker's audience, so it proves nothing; name a dedicated account", setting, text)
}

// podAccountRefusal refuses, naming setting and the entry text, an account no exchange entry for
// a pod, no BROKER_POD_OPERATORS entry and no side of a session account pair may name: one that
// is not AccountForm, and a namespace's default account.
func podAccountRefusal(setting, text, account string) error {
	switch {
	case !oauthwire.AccountSubject.MatchString(account):
		return fmt.Errorf("%s must name %s accounts, got %q (%w)", setting, AccountForm, text, identity.ErrAccountForm)
	case isDefaultServiceAccount(account):
		return fmt.Errorf("%s names a namespace's default service account, got %q: every pod in that namespace whose spec names no account runs as it, so its token says nothing about which pod is asking; name a dedicated account", setting, text)
	}
	return nil
}

// ParsePodAccount reads account as a pod's, for the settings that name one outside this package
// (BROKER_SERVICES, BROKER_SERVICE_LOGINS, BROKER_POD_OPERATORS, BROKER_SESSION_ACCOUNT_PAIRS,
// BROKER_POD_REGISTRARS): the subject as given, refused in podAccountRefusal's words.
func ParsePodAccount(setting, text, account string) (string, error) {
	if err := podAccountRefusal(setting, text, account); err != nil {
		return "", err
	}
	return account, nil
}

// parseAccounts reads setting's whitespace-separated
// <service account>=<project>=<person>[=<session>] entries, the identities BROKER_EXCHANGE_ACCOUNTS
// lists, in order. The project may be empty (<service account>==<person>), which is the shared
// public-session account's entry: its named tokens act for the person and name no project. The
// person may be empty with it (<service account>==), an account whose tokens act as the
// account itself. It refuses, naming the entry: one not of that form, with a blank account, or
// naming a project and no person, since an entry that names a project names its person, so no
// account entry's token that names a project is personless; an account that is not AccountForm,
// or a namespace's default account (podAccountRefusal); and an account listed twice, since one
// account writes as one identity. New is the second gate, and the session name's own form is its
// to refuse.
func parseAccounts(setting, value string) ([]AccountEntry, error) {
	var entries []AccountEntry
	seen := map[string]bool{}
	for _, text := range strings.Fields(value) {
		fields, ok := exchange.EntryFields(text, 4)
		if !ok || fields[0] == "" {
			return nil, fmt.Errorf("%s must be <service account>=<project>=<person>[=<session>] entries, the project empty for an entry naming none, the person empty only beside it, and the session everything after the third =, got %q", setting, text)
		}
		if fields[1] != "" && fields[2] == "" {
			return nil, fmt.Errorf("%s names a project and no person in %q; an entry names no person only when it names no project either (<service account>==): an entry that names a project names its person, so no account entry's token that names a project is personless", setting, text)
		}
		if err := podAccountRefusal(setting, text, fields[0]); err != nil {
			return nil, err
		}
		if seen[fields[0]] {
			return nil, fmt.Errorf("%s names the service account %s twice; one account writes as one identity, so name it once", setting, fields[0])
		}
		seen[fields[0]] = true
		entries = append(entries, AccountEntry{Account: fields[0], Project: fields[1], Person: fields[2], Session: fields[3]})
	}
	return entries, nil
}

// parseReadAccounts reads setting's whitespace-separated <service account>=<service>=<session>
// entries, the read entries BROKER_EXCHANGE_READ_ACCOUNTS lists, in order. Every field is
// required and the session is everything after the second "=", so it alone may carry one. It
// refuses, naming the setting and the entry: one with fewer than three fields or a blank one; an
// account that is not AccountForm, or a namespace's default account (podAccountRefusal); a
// service that is not a service's name, or is policy.OwnerShared; and an account listed twice,
// since one account reads as one identity. An account BROKER_EXCHANGE_ACCOUNTS lists too is
// listingRefusal's to refuse, since it reads both settings.
func parseReadAccounts(setting, value string) ([]ReadEntry, error) {
	var entries []ReadEntry
	seen := map[string]bool{}
	for _, text := range strings.Fields(value) {
		// An entry with no session comes back with a blank one (exchange.EntryFields), which the
		// blank check refuses as it refuses any other blank field: a read entry's session is
		// required.
		fields, ok := exchange.EntryFields(text, 3)
		if !ok || fields[0] == "" || fields[1] == "" || fields[2] == "" {
			return nil, fmt.Errorf("%s must be <service account>=<service>=<session> entries, every field required and the session everything after the second =, got %q", setting, text)
		}
		account, service, session := fields[0], fields[1], fields[2]
		if err := podAccountRefusal(setting, text, account); err != nil {
			return nil, err
		}
		if !record.ValidService(service) || service == policy.OwnerShared {
			return nil, fmt.Errorf("%s must name services of lowercase letters, digits and hyphens, at most 64, not %s, got %q", setting, policy.OwnerShared, text)
		}
		if seen[account] {
			return nil, fmt.Errorf("%s names the service account %s twice, got %q again; one account reads as one identity, so name it once", setting, account, text)
		}
		seen[account] = true
		entries = append(entries, ReadEntry{Account: account, Service: service, Session: session})
	}
	return entries, nil
}

// parseJobAccounts reads setting's whitespace-separated <service account>=<projects>[=<session>]
// entries, the job accounts BROKER_EXCHANGE_JOB_ACCOUNTS lists, in order, each with its projects
// as Job and no person and no project. It refuses what exchange.ParseJobEntries refuses, an
// account podAccountRefusal refuses among them. An account another exchange setting lists too is
// listingRefusal's to refuse, since it reads them all. New is the second gate, and the session
// name's own form is its to refuse.
func parseJobAccounts(setting, value string) ([]AccountEntry, error) {
	parsed, err := exchange.ParseJobEntries(setting, value, "<service account>=<projects>[=<session>]", podAccountRefusal)
	if err != nil {
		return nil, err
	}
	var entries []AccountEntry
	for _, entry := range parsed {
		entries = append(entries, AccountEntry{Account: entry.Account, Job: entry.Job, Session: entry.Session})
	}
	return entries, nil
}

// listingRefusal refuses a Kubernetes service account that BROKER_EXCHANGE_ACCOUNTS (accounts),
// BROKER_EXCHANGE_READ_ACCOUNTS (reads) and BROKER_EXCHANGE_JOB_ACCOUNTS (jobs) list more than
// once between them. One account has one answer: listed in two settings, it would mint two kinds
// of token. New refuses it too, but only this refusal can name both settings. Each setting's
// parser refuses an account that setting lists twice, so a second listing is in a later setting:
// the refusal names that setting, then the one that lists the account first, then the account,
// and says why a token of the later setting cannot share it. LoadConfig calls it once, after it
// reads all three settings.
func listingRefusal(accounts []AccountEntry, reads []ReadEntry, jobs []AccountEntry) error {
	// listedBy is the setting that lists each account.
	listedBy := make(map[string]string, len(accounts))
	for _, entry := range accounts {
		listedBy[entry.Account] = "BROKER_EXCHANGE_ACCOUNTS"
	}
	// list indexes account by setting, and refuses it when an earlier setting lists it, with
	// reason, which says why a token of setting cannot share it.
	list := func(setting, account, reason string) error {
		if first, listed := listedBy[account]; listed {
			return fmt.Errorf("%s and %s both name the service account %s; %s, so name it in one of them", setting, first, account, reason)
		}
		listedBy[account] = setting
		return nil
	}
	for _, entry := range reads {
		if err := list("BROKER_EXCHANGE_READ_ACCOUNTS", entry.Account, "one account mints one kind of token, a read-only one or one that writes as its person or as the account"); err != nil {
			return err
		}
	}
	for _, entry := range jobs {
		if err := list("BROKER_EXCHANGE_JOB_ACCOUNTS", entry.Account, "one account mints one kind of token, and only a job's names the account as its service and carries projects"); err != nil {
			return err
		}
	}
	return nil
}
