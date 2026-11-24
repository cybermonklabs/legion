package google

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/policy"
	"github.com/sjawhar/envoy/internal/cipher/record"
	"github.com/sjawhar/envoy/internal/cipher/secrets"
)

// AccountTag opens the account of a setting's entry whose caller presents a Google identity
// token. A Kubernetes entry already names its own kind — every such subject begins
// system:serviceaccount: — so tagging this one leaves every entry written today unchanged and
// still lets a reader, and the parser, tell the two kinds apart by the entry's own text.
const AccountTag = "google:"

// AccountForm is how a Google account is written wherever the broker's settings name one.
const AccountForm = AccountTag + "<service account email>"

// DefaultIssuer is the issuer of every Google identity token, the one a machine's metadata server
// has Google sign, and what BROKER_GOOGLE_OIDC_ISSUER is when the environment names none. The
// variable exists because a dev stack has no Google: cmd/dev-oidc-issuer stands in for it on a
// loopback port, so the broker and its machines can be driven end to end on one machine
// (scripts/dev-stack.sh). A deployment leaves it unset and gets Google.
const DefaultIssuer = "https://accounts.google.com"

// googleIDPart is a Google project or account id: a lowercase letter, then 4 to 28 of lowercase
// letters, digits and hyphens, then a lowercase letter or digit, so 6 to 30 characters that never
// end in a hyphen.
const googleIDPart = `[a-z][a-z0-9-]{4,28}[a-z0-9]`

// projectIDPattern is a Google project ID.
var projectIDPattern = regexp.MustCompile(`^` + googleIDPart + `$`)

// serviceAccountPattern is a user-managed Google service account's email,
// <account id>@<project id>.iam.gserviceaccount.com, the only form a VM service may name.
var serviceAccountPattern = regexp.MustCompile(`^` + googleIDPart + `@` + googleIDPart + `\.iam\.gserviceaccount\.com$`)

// defaultServiceAccountPattern is a project's Compute Engine default account
// (<project number>-compute@developer.gserviceaccount.com) or its App Engine default account
// (<project id>@appspot.gserviceaccount.com). A project's workloads run as them by default, so a
// token from one proves nothing about which VMs are a service's.
var defaultServiceAccountPattern = regexp.MustCompile(`^([0-9]+-compute@developer|` + googleIDPart + `@appspot)\.gserviceaccount\.com$`)

// serviceAgentPattern is a Google-managed service agent: an account named service-<number>,
// service-org-<number> or service-folder-<number>, or any account of a gcp-sa- domain. Google's
// own services run as these accounts, never a VM.
var serviceAgentPattern = regexp.MustCompile(`^service-(org-|folder-)?[0-9]+@|@gcp-sa-`)

// instancePatternChars are the characters an instance-name glob may hold: an instance name's own
// (lowercase letters, digits and hyphens) and path.Match's *, ?, [, ] and ^ (validInstancePattern
// takes ^ only where it negates a class). Any other character could never match an instance name,
// and a ! would read as negation to someone who knows shell globs, where path.Match takes it
// literally.
var instancePatternChars = regexp.MustCompile(`^[a-z0-9*?\[\]^-]+$`)

// LoadConfig reads this provider's own variables from getenv, in the order config.Load read them:
// BROKER_GOOGLE_OIDC_ISSUER and BROKER_GOOGLE_OIDC_AUDIENCE, then, each refused without that
// audience, BROKER_SIGNIN_DEVICE_AUDIENCE, BROKER_VM_SERVICES, BROKER_VM_PROJECTS,
// BROKER_VM_INSTANCES, BROKER_VM_EXCHANGE_ACCOUNTS and BROKER_VM_EXCHANGE_JOB_ACCOUNTS, and last
// BROKER_NATS_GOOGLE_AUDIENCE. Every refusal is the one config.Load answered, word for word.
// BROKER_SIGNIN_DOMAIN is the broker's own setting, which config fills in (Config.SignInDomain).
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		Issuer:         orDefault(strings.TrimSpace(getenv("BROKER_GOOGLE_OIDC_ISSUER")), DefaultIssuer),
		Audience:       strings.TrimSpace(getenv("BROKER_GOOGLE_OIDC_AUDIENCE")),
		PersonAudience: strings.TrimSpace(getenv("BROKER_SIGNIN_DEVICE_AUDIENCE")),
		NATSAudience:   strings.TrimSpace(getenv("BROKER_NATS_GOOGLE_AUDIENCE")),
	}
	if c.Audience == "" {
		for _, setting := range []string{"BROKER_VM_SERVICES", "BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES", "BROKER_VM_EXCHANGE_ACCOUNTS", "BROKER_VM_EXCHANGE_JOB_ACCOUNTS", "BROKER_SIGNIN_DEVICE_AUDIENCE"} {
			if strings.TrimSpace(getenv(setting)) != "" {
				return Config{}, fmt.Errorf("%s needs BROKER_GOOGLE_OIDC_AUDIENCE, whose Google identity tokens it configures", setting)
			}
		}
	}
	services, err := parseServiceAccounts("BROKER_VM_SERVICES", getenv("BROKER_VM_SERVICES"))
	if err != nil {
		return Config{}, err
	}
	if c.PersonAudience != "" && len(services) == 0 {
		return Config{}, fmt.Errorf("BROKER_SIGNIN_DEVICE_AUDIENCE needs BROKER_VM_SERVICES: a person's device sign-in requires a listed machine's Google identity token as its actor")
	}
	for _, entry := range services {
		if c.ServicesByAccount == nil {
			c.ServicesByAccount = map[string]string{}
		}
		c.ServicesByAccount[entry.account] = entry.service
	}
	projects, err := parseAccountValues("BROKER_VM_PROJECTS", getenv("BROKER_VM_PROJECTS"), "project id", projectIDPattern.MatchString)
	if err != nil {
		return Config{}, err
	}
	instances, err := parseAccountValues("BROKER_VM_INSTANCES", getenv("BROKER_VM_INSTANCES"), "instance-name glob", validInstancePattern)
	if err != nil {
		return Config{}, err
	}
	// BROKER_VM_PROJECTS and BROKER_VM_INSTANCES are locals of LoadConfig: the entry each one
	// pins (Machines' CloudProject and Instance, built below) is the only thing that reads them,
	// and no field holds them. What each one means is documented in cmd/cipherd, because
	// cipher-refgen takes a setting's description from a Config field's own comment or from a
	// comment in cmd/cipherd, and these two have no field.
	pinnedProjects, err := accountMap("BROKER_VM_PROJECTS", projects, c.ServicesByAccount)
	if err != nil {
		return Config{}, err
	}
	pinnedInstances, err := accountMap("BROKER_VM_INSTANCES", instances, c.ServicesByAccount)
	if err != nil {
		return Config{}, err
	}
	grants, err := parseExchangeGrants("BROKER_VM_EXCHANGE_ACCOUNTS", getenv("BROKER_VM_EXCHANGE_ACCOUNTS"))
	if err != nil {
		return Config{}, err
	}
	granted := map[string]exchangeGrant{}
	for _, grant := range grants {
		if _, listed := c.ServicesByAccount[grant.account]; !listed {
			return Config{}, fmt.Errorf("BROKER_VM_EXCHANGE_ACCOUNTS names %s, got %q, but BROKER_VM_SERVICES does not list it; a person and a project are given to a listed VM account", grant.account, grant.text)
		}
		granted[grant.account] = grant
	}
	// BROKER_VM_EXCHANGE_JOB_ACCOUNTS gives job lists to the accounts BROKER_VM_SERVICES lists:
	// an account it names that is not listed, or that BROKER_VM_EXCHANGE_ACCOUNTS names too, is
	// refused here, and New is the second gate.
	jobGrants, err := exchange.ParseJobEntries("BROKER_VM_EXCHANGE_JOB_ACCOUNTS", getenv("BROKER_VM_EXCHANGE_JOB_ACCOUNTS"), "<service account email>=<projects>[=<session>]", AccountRefusal)
	if err != nil {
		return Config{}, err
	}
	jobGranted := map[string]exchange.JobEntry{}
	for _, grant := range jobGrants {
		if _, listed := c.ServicesByAccount[grant.Account]; !listed {
			return Config{}, fmt.Errorf("BROKER_VM_EXCHANGE_JOB_ACCOUNTS names %s, got %q, but BROKER_VM_SERVICES does not list it; a job's projects are given to a listed VM account", grant.Account, grant.Text)
		}
		// One account has one answer: named in both settings, its machines would act for a
		// person and be a job that names none. New refuses the entry too, but only this refusal
		// can name both settings.
		if person, named := granted[grant.Account]; named {
			return Config{}, fmt.Errorf("BROKER_VM_EXCHANGE_JOB_ACCOUNTS and BROKER_VM_EXCHANGE_ACCOUNTS both name the service account %s, got %q and %q; one account has one answer, a token that acts for its person or a job's that names none, so name it in one of them", grant.Account, grant.Text, person.text)
		}
		jobGranted[grant.Account] = grant
	}
	for _, entry := range services {
		// Each pin is optional and the two go together: an account pinned to a project but no
		// instance name, or the other way round, says nothing clear about which machines it
		// means, so it is refused here rather than read as either "every instance of that
		// project" or "that name in any project".
		project, pinnedProject := pinnedProjects[entry.account]
		instance, pinnedInstance := pinnedInstances[entry.account]
		if pinnedProject != pinnedInstance {
			named, missing := "BROKER_VM_PROJECTS", "BROKER_VM_INSTANCES"
			if pinnedInstance {
				named, missing = missing, named
			}
			return Config{}, fmt.Errorf("%s names %s but %s does not, got %q: an account is pinned to a Compute Engine project and an instance-name pattern together, or to neither, so that which machines it takes is clear", named, entry.account, missing, entry.text)
		}
		machine := GoogleEntry{Account: entry.account, Service: entry.service, CloudProject: project, Instance: instance}
		if grant, found := granted[entry.account]; found {
			machine.Project, machine.Person, machine.Session = grant.project, grant.person, grant.session
		}
		if grant, found := jobGranted[entry.account]; found {
			machine.Job, machine.Session = grant.Job, grant.Session
		}
		c.Machines = append(c.Machines, machine)
	}
	return c, nil
}

// Serves is the sentence a Google account's site needs when this provider verifies no caller of
// use: such a caller could never authenticate without the audience its token must carry, so the
// start is refused where the broker's settings name one. It answers nil where that setting is set.
func (c Config) Serves(use identity.Use, site identity.Site) error {
	switch {
	case use == identity.UseAPI && c.Audience == "" && site.Edge:
		return fmt.Errorf("%s entry %q names a Google service account as an edge listener, which needs BROKER_GOOGLE_OIDC_AUDIENCE, the audience its Google identity token must carry", site.Setting, site.Entry)
	case use == identity.UseAPI && c.Audience == "":
		return fmt.Errorf("%s names a Google service account, which needs BROKER_GOOGLE_OIDC_AUDIENCE, the audience its caller's Google identity token must carry", site.Setting)
	case use == identity.UseNATS && c.NATSAudience == "":
		return fmt.Errorf("%s lists %s, a Google account, and BROKER_NATS_GOOGLE_AUDIENCE is unset, so its token could not be verified", site.Setting, site.Entry)
	}
	return nil
}

// Check is this provider's rule against what the broker's own settings name: an audience with no
// Google caller of any kind would read Google's keys at boot and then accept no token at all.
// callers is whether BROKER_API_ACCOUNTS or BROKER_NATS_SERVICES names a Google account.
func (c Config) Check(callers bool) error {
	if c.Audience != "" && !callers && len(c.ServicesByAccount) == 0 {
		return fmt.Errorf("BROKER_GOOGLE_OIDC_AUDIENCE needs a Google caller: a google: entry in BROKER_API_ACCOUNTS, a VM service in BROKER_VM_SERVICES or a google: edge-listener entry in BROKER_NATS_SERVICES; with none, no Google identity token for that audience could ever be accepted")
	}
	return nil
}

// Mentions is every place this provider's settings name account: its VM service and the person
// its exchange entry gives it, or the job list its job entry gives it.
func (c Config) Mentions(account identity.Account) []identity.Mention {
	if account.Kind != Kind {
		return nil
	}
	var mentions []identity.Mention
	for _, entry := range c.Machines {
		if entry.Account != account.Address {
			continue
		}
		if entry.Job.Listed() {
			mentions = append(mentions, identity.Mention{Variable: "BROKER_VM_EXCHANGE_JOB_ACCOUNTS", Service: entry.Service, Job: entry.Job})
			continue
		}
		mentions = append(mentions, identity.Mention{Variable: "BROKER_VM_EXCHANGE_ACCOUNTS", Person: entry.Person, Service: entry.Service})
	}
	return mentions
}

// Services is every service BROKER_VM_SERVICES names, which the policy loader registers as an
// owner a secret may name.
func (c Config) Services() []string {
	return slices.Sorted(maps.Values(c.ServicesByAccount))
}

// ParseAccount reads account written AccountForm, answering false for text of another form, which
// another provider may claim. It refuses, naming setting and the entry text it came from, every
// address AccountRefusal refuses, the rule BROKER_VM_SERVICES holds every Google account to.
func ParseAccount(setting, text, account string) (identity.Account, bool, error) {
	address, tagged := strings.CutPrefix(account, AccountTag)
	if !tagged {
		return identity.Account{}, false, nil
	}
	if err := AccountRefusal(setting, text, address); err != nil {
		return identity.Account{}, true, err
	}
	return identity.Account{Kind: Kind, Address: address}, true, nil
}

// AccountRefusal refuses, naming setting and the entry text, an account no VM service may name: a
// user-managed account's address written with capital letters (a Google identity token carries it
// in lowercase), a project's default account, a Google-managed service agent, and any address
// that is not a user-managed service account's. It answers nil for an account a VM service may
// name.
func AccountRefusal(setting, text, account string) error {
	switch {
	case account != strings.ToLower(account) && serviceAccountPattern.MatchString(strings.ToLower(account)):
		return fmt.Errorf("%s names a service account with capital letters, got %q: a Google identity token carries the address in lowercase, so write it in lowercase", setting, text)
	case defaultServiceAccountPattern.MatchString(account):
		return fmt.Errorf("%s names a project's default service account, got %q: the project's workloads run as it by default, so it proves nothing about which VMs are a service's; name a dedicated account", setting, text)
	case secrets.IsServiceAgent(account):
		return fmt.Errorf("%s names a Google-managed service agent, got %q: Google's own services run as it, never a VM; name a dedicated account", setting, text)
	case !serviceAccountPattern.MatchString(account):
		return fmt.Errorf("%s must name user-managed service accounts, <account id>@<project id>.iam.gserviceaccount.com, got %q", setting, text)
	}
	return nil
}

// serviceAccountEntry is one <service>=<service account email> entry of BROKER_VM_SERVICES, with
// its text as the setting gave it.
type serviceAccountEntry struct{ service, account, text string }

// parseServiceAccounts reads setting's whitespace-separated <service>=<service account email>
// entries, in order. It refuses, naming the entry: one not of that form or naming the service
// policy.OwnerShared; an account AccountRefusal refuses; and an account given twice, since each
// account proves one service. A service named for several accounts is allowed.
func parseServiceAccounts(setting, value string) ([]serviceAccountEntry, error) {
	var entries []serviceAccountEntry
	seen := map[string]string{}
	for _, text := range strings.Fields(value) {
		service, account, ok := strings.Cut(text, "=")
		if !ok || !record.ValidService(service) || service == policy.OwnerShared {
			return nil, fmt.Errorf("%s must be <service>=<service account email> entries, each service lowercase letters, digits and hyphens, at most 64, not %s, got %q", setting, policy.OwnerShared, text)
		}
		if err := AccountRefusal(setting, text, account); err != nil {
			return nil, err
		}
		if other, twice := seen[account]; twice {
			if other == service {
				return nil, fmt.Errorf("%s names the service account %s twice for %q; each account proves one service, so name it once", setting, account, service)
			}
			return nil, fmt.Errorf("%s names the service account %s for both %q and %q; each account proves one service, so name it once", setting, account, other, service)
		}
		seen[account] = service
		entries = append(entries, serviceAccountEntry{service: service, account: account, text: text})
	}
	return entries, nil
}

// accountValue is one <service account email>=<value> entry of BROKER_VM_PROJECTS or
// BROKER_VM_INSTANCES, with its text as the setting gave it.
type accountValue struct{ account, value, text string }

// parseAccountValues reads setting's whitespace-separated <service account email>=<kind> entries,
// in order. It refuses, naming the entry: one not of that form or whose value valid refuses; an
// account AccountRefusal refuses; and an account given twice, since a VM identity must have one
// value of each kind.
func parseAccountValues(setting, value, kind string, valid func(string) bool) ([]accountValue, error) {
	var entries []accountValue
	seen := map[string]bool{}
	for _, text := range strings.Fields(value) {
		account, attribute, ok := strings.Cut(text, "=")
		if !ok || !valid(attribute) {
			return nil, fmt.Errorf("%s must be <service account email>=<%s> entries, got %q", setting, kind, text)
		}
		if err := AccountRefusal(setting, text, account); err != nil {
			return nil, err
		}
		if seen[account] {
			return nil, fmt.Errorf("%s names the service account %s twice; each account has one %s, so name it once", setting, account, kind)
		}
		seen[account] = true
		entries = append(entries, accountValue{account: account, value: attribute, text: text})
	}
	return entries, nil
}

// validInstancePattern reports whether pattern is an instance-name glob: path.Match's syntax,
// holding only instancePatternChars, with ^ only as a class's negation ("[^"), the one place
// path.Match does not take it as a literal no instance name holds.
func validInstancePattern(pattern string) bool {
	_, err := path.Match(pattern, "")
	return instancePatternChars.MatchString(pattern) && strings.Count(pattern, "^") == strings.Count(pattern, "[^") && err == nil
}

// accountMap keys entries by account, refusing, naming setting and the entry, one whose account
// services does not bind: a project or an instance pattern for an account no VM service lists
// would configure nothing. No entries answer nil, as an unset setting does.
func accountMap(setting string, entries []accountValue, services map[string]string) (map[string]string, error) {
	var values map[string]string
	for _, entry := range entries {
		if _, listed := services[entry.account]; !listed {
			return nil, fmt.Errorf("%s names %s, got %q, but BROKER_VM_SERVICES does not list it; a project or instance entry must name a listed VM account", setting, entry.account, entry.text)
		}
		if values == nil {
			values = map[string]string{}
		}
		values[entry.account] = entry.value
	}
	return values, nil
}

// exchangeGrant is one BROKER_VM_EXCHANGE_ACCOUNTS entry: the account it names, the Dispatch
// project its tokens write, the person they act for, the session name it fixes, if any, and its
// text as the setting gave it.
type exchangeGrant struct{ account, project, person, session, text string }

// parseExchangeGrants reads setting's whitespace-separated
// <service account email>=<project>=<person>[=<session>] entries, the grants
// BROKER_VM_EXCHANGE_ACCOUNTS gives the accounts BROKER_VM_SERVICES lists, in order. It refuses,
// naming the entry: one not of that form or with a blank field; an account AccountRefusal
// refuses; and an account named twice, since one account writes as one identity. Whether the
// account is listed at all is LoadConfig's to refuse, as it is for a project or an instance
// pattern (accountMap), and New is the second gate.
func parseExchangeGrants(setting, value string) ([]exchangeGrant, error) {
	var grants []exchangeGrant
	seen := map[string]bool{}
	for _, text := range strings.Fields(value) {
		fields, ok := exchange.EntryFields(text, 4)
		if !ok || fields[0] == "" || fields[1] == "" || fields[2] == "" {
			return nil, fmt.Errorf("%s must be <service account email>=<project>=<person>[=<session>] entries, the session everything after the third =, got %q", setting, text)
		}
		if err := AccountRefusal(setting, text, fields[0]); err != nil {
			return nil, err
		}
		if seen[fields[0]] {
			return nil, fmt.Errorf("%s names the service account %s twice; one account writes as one identity, so name it once", setting, fields[0])
		}
		seen[fields[0]] = true
		grants = append(grants, exchangeGrant{account: fields[0], project: fields[1], person: fields[2], session: fields[3], text: text})
	}
	return grants, nil
}

// orDefault is v, or d when v is empty.
func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
