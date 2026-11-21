package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/modelpace"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/wordrecords"
	"github.com/sjawhar/envoy/internal/dispatch/writingcheck"
)

// writing_settings.go reads the settings of the work on agents' texts (INFRA-3231): the reviewer,
// Rewrite and Define, the records of the words people ask about, the written rules' refusal, the
// three model groups those call, the deployments' quotas and a test process's stand-in model. It
// reads them into writingSettings, and a model group into the model it calls and its share of the
// pacer (modelGroup.agentloopModel, modelGroup.pace); it calls nothing and reads no file, so a
// model service that is down, or a key file not written yet, never refuses boot.

// The kinds of a model group.
const (
	// modelKindAzure is a chat deployment of an Azure AI Foundry resource, signed in to with the
	// shared Azure sign-in, as the answerer's DISPATCH_ANSWERER=azure is.
	modelKindAzure = "azure"
	// modelKindAnthropic is an Anthropic Messages service, such as the operator's model gateway,
	// which takes the key a file holds.
	modelKindAnthropic = "anthropic"
)

// modelGroupSpec is one model group's settings: the prefix of its settings, whose group it is and
// what its calls are, as a refusal names them, what each of its calls is charged on the pacer at
// the least (modelpace.Charge), which a minute's quota must be more than (refuseQuotaNoCallFits):
// the reply bound of its calls, the most tokens one reply may hold, and the shortest system prompt
// of the calls it sends under the reviewer's rule sets (leastPrompt); and its group of a
// writingSettings, nil for the test models, which are no field of their own.
type modelGroupSpec struct {
	prefix, whose, calls string
	replyBound           int
	leastPrompt          func(rules []reviewer.RuleSet) (int, error)
	group                func(cfg *writingSettings) *modelGroup
}

// The three model groups, and each kind's test models. The reviewer's group sends reviews, Rewrite
// and Define's rewrites and definitions, and a test model, which a request names in place of
// either group, all three.
var (
	reviewerModelSpec = modelGroupSpec{"DISPATCH_REVIEWER_MODEL_", "the reviewer's", "reviews", reviewer.DefaultOutputTokens,
		reviewerPrompts(reviewer.PurposeReview), func(cfg *writingSettings) *modelGroup { return &cfg.ReviewerModel }}
	rewriteDefineModelSpec = modelGroupSpec{"DISPATCH_REWRITE_DEFINE_MODEL_", "Rewrite and Define's", "rewrites and definitions", reviewer.DefaultOutputTokens,
		reviewerPrompts(reviewer.PurposeRewrite, reviewer.PurposeDefine), func(cfg *writingSettings) *modelGroup { return &cfg.RewriteDefineModel }}
	wordRecordsModelSpec = modelGroupSpec{"DISPATCH_WORD_RECORDS_MODEL_", "the records job's", "calls", wordrecords.CallOutputTokens,
		selectPromptRunes, func(cfg *writingSettings) *modelGroup { return &cfg.WordRecordsModel }}
	// modelGroupSpecs is the three model groups, in the order a refusal weighs them.
	modelGroupSpecs = []modelGroupSpec{reviewerModelSpec, rewriteDefineModelSpec, wordRecordsModelSpec}
)

// reviewerPrompts is the shortest system prompt of the reviewer's calls of purposes under rules
// (reviewer.LeastSystemPromptRunes).
func reviewerPrompts(purposes ...string) func([]reviewer.RuleSet) (int, error) {
	return func(rules []reviewer.RuleSet) (int, error) {
		return reviewer.LeastSystemPromptRunes(rules, purposes...)
	}
}

// selectPromptRunes is the records job's one system prompt's characters (wordrecords.SelectPrompt),
// which no rule set changes.
func selectPromptRunes([]reviewer.RuleSet) (int, error) {
	return utf8.RuneCountInString(wordrecords.SelectPrompt), nil
}

// testModelSpec is the spec of the test models of kind (DISPATCH_TEST_MODELS_<KIND>_*).
func testModelSpec(kind string) modelGroupSpec {
	return modelGroupSpec{testModelPrefixes[kind], testModelsWhose(kind), "reviews, rewrites and definitions", reviewer.DefaultOutputTokens,
		reviewerPrompts(reviewer.PurposeReview, reviewer.PurposeRewrite, reviewer.PurposeDefine), nil}
}

// leastCharge is the least any call of the group is charged on the pacer under rules: its
// shortest system prompt and its reply bound (modelpace.Charge).
func (s modelGroupSpec) leastCharge(rules []reviewer.RuleSet) (int, error) {
	least, err := s.leastPrompt(rules)
	if err != nil {
		return 0, fmt.Errorf("%s %s' shortest system prompt: %w", s.whose, s.calls, err)
	}
	return modelpace.Charge(least, s.replyBound), nil
}

// The reply bounds of the groups, and the least their calls are charged under the default rule
// sets, as the settings table's descriptions state them.
var (
	reviewerReplyBound       = strconv.Itoa(reviewerModelSpec.replyBound)
	recordsReplyBound        = strconv.Itoa(wordRecordsModelSpec.replyBound)
	reviewerLeastCharge      = leastChargeText(reviewerModelSpec)
	rewriteDefineLeastCharge = leastChargeText(rewriteDefineModelSpec)
	recordsLeastCharge       = leastChargeText(wordRecordsModelSpec)
	testModelsLeastCharge    = leastChargeText(testModelSpec(modelKindAzure))
)

// leastChargeText is the least the calls of spec are charged under reviewer.DefaultRules.
func leastChargeText(spec modelGroupSpec) string {
	charge, err := spec.leastCharge(reviewer.DefaultRules)
	if err != nil {
		panic(err)
	}
	return strconv.Itoa(charge)
}

// writingRulesExtraSets are the rule sets DISPATCH_WRITING_RULES_EXTRA may name, the ones the
// server's writing rules show (api.WritingRulesConfig).
var writingRulesExtraSets = []reviewer.RuleSet{reviewer.RuleReaderWords, reviewer.RuleAskParts}

// ruleSetNames is the names of sets, as a setting spells them.
func ruleSetNames(sets ...reviewer.RuleSet) []string {
	names := make([]string, len(sets))
	for i, set := range sets {
		names[i] = string(set)
	}
	return names
}

// defaultWordRecordsFrom is DISPATCH_WORD_RECORDS_FROM unset: the first event of the event log the
// records job's backfill reads.
const defaultWordRecordsFrom = "2026-10-16T00:00:00Z"

// writingSettings are the settings of the work on agents' texts, as resolveWritingSettings
// checked them. Every default keeps each change to a text off.
type writingSettings struct {
	// The reviewer's choice for each kind of text an agent writes: api.ReviewOff, api.ReviewAfter
	// or, for every kind but a document version, api.ReviewHold.
	ReviewAsks           string
	ReviewIssues         string
	ReviewComments       string
	ReviewMessages       string
	ReviewDocuments      string
	ReviewDecisionBlocks string
	// ReviewerRules are the rule sets the reviewer, Rewrite and Define apply
	// (DISPATCH_REVIEWER_RULES).
	ReviewerRules []reviewer.RuleSet
	// ReviewerTimeout bounds the reviewer's model call (DISPATCH_REVIEWER_TIMEOUT).
	ReviewerTimeout time.Duration
	// ReviewerWriterWindow is the time a writer session has to act on a held text
	// (DISPATCH_REVIEWER_WRITER_WINDOW).
	ReviewerWriterWindow time.Duration
	// ReviewerUnedited is api.UneditedRewrite or api.UneditedAsWritten (DISPATCH_REVIEWER_UNEDITED).
	ReviewerUnedited string
	// The three model groups. A group whose Kind is "" is unset: no model.
	ReviewerModel      modelGroup
	RewriteDefineModel modelGroup
	WordRecordsModel   modelGroup
	// WordRecords runs the records job while WordRecordsModel is set (DISPATCH_WORD_RECORDS).
	WordRecords bool
	// WordRecordsFrom is the time the records job's first run reads from
	// (DISPATCH_WORD_RECORDS_FROM).
	WordRecordsFrom time.Time
	// WritingRulesRefusal turns on the dispatch command's refusal of R6
	// (DISPATCH_WRITING_RULES_REFUSAL), and WritingRulesExtra are the rule sets it shows beside
	// the written rules (DISPATCH_WRITING_RULES_EXTRA).
	WritingRulesRefusal bool
	WritingRulesExtra   []reviewer.RuleSet
	// DeploymentQuotas is each deployment's tokens-per-minute quota by its name
	// (DISPATCH_MODEL_DEPLOYMENT_QUOTAS), which the process's pacer (modelpace) takes.
	DeploymentQuotas map[string]int
	// TestModelURL is DISPATCH_TEST_MODEL_URL, read only under the test hooks: the stand-in
	// model every set group's Endpoint is.
	TestModelURL string
	// TestModels are the models a test request may name in its Dispatch-Test-Model header
	// (DISPATCH_TEST_MODELS), read only under the test hooks, by name: each is the group of its
	// kind's DISPATCH_TEST_MODELS_<KIND>_* settings with the name's id as its Deployment and the
	// name's effort, if it names one, as its Effort.
	TestModels map[string]modelGroup
}

// modelGroup is one model group as resolveModelGroup checked it. Kind is "" where the group is
// unset; the rest belongs to a set group, Kind's fields alone.
type modelGroup struct {
	Kind string
	// Endpoint is the base URL the kind's route is appended to: the group's own, or the stand-in
	// model's (StandIn).
	Endpoint string
	// Deployment is what each request names as its model: the Azure deployment's name or the
	// Anthropic model's id.
	Deployment string
	// APIVersion is the api-version query value of an azure group's requests, "" for none.
	APIVersion string
	// Scope is the token scope an azure group's sign-in asks for.
	Scope string
	// KeyFile is the file holding an anthropic group's key, read on each call.
	KeyFile string
	// TokensPerMinute is the group's ceiling on its share of its deployment's quota.
	TokensPerMinute int
	// TenantID, ClientID and FederatedToken are the shared Azure sign-in an azure group signs in
	// with.
	TenantID       string
	ClientID       string
	FederatedToken string
	// StandIn says Endpoint is DISPATCH_TEST_MODEL_URL's stand-in, called with no credential.
	StandIn bool
	// Effort is the reasoning effort a test model's name asks for (`<id>:<effort>`,
	// DISPATCH_TEST_MODELS), one of testModelEfforts. It is "" for a model group and for a test
	// model named by its id alone, which ask for their kind's own (agentloopModel).
	Effort string
}

// standInToken is the credential every call to the stand-in model presents (DISPATCH_TEST_MODEL_URL),
// which reads none.
const standInToken = "stand-in"

// agentloopModel is the model a set group calls through agentloop: an azure group's chat
// deployment (agentloop.OpenAIChat), its api-version on every request and a token of the shared
// Azure sign-in at its scope; an anthropic group's Messages service (agentloop.AnthropicMessages),
// its key file read on each call, so a key rewritten under the process is the next call's. A group
// beside the stand-in model presents no credential of its own. An anthropic group asks for no
// thinking (effort "off"). An azure group sends no reasoning effort at all: agentloop sends "off"
// as `none`, which a Foundry chat deployment refuses (measured on grok-4.7, as the detector's
// reader found: writingcheck.ReaderNoEffort). A test model whose name asks for an effort
// (`<id>:<effort>`, DISPATCH_TEST_MODELS) asks for that one on either kind. It reads no file and
// calls nothing.
func (g modelGroup) agentloopModel() (agentloop.Model, error) {
	var token func(context.Context) (string, error)
	switch {
	case g.StandIn:
		token = func(context.Context) (string, error) { return standInToken, nil }
	case g.Kind == modelKindAzure:
		source, err := embed.NewEntraTokenSource(embed.EntraConfig{
			TenantID: g.TenantID, ClientID: g.ClientID, TokenFile: g.FederatedToken, Scope: g.Scope,
		})
		if err != nil {
			return agentloop.Model{}, err
		}
		token = source.Token
	case g.Kind == modelKindAnthropic:
		keyFile := g.KeyFile
		token = func(context.Context) (string, error) {
			key, err := os.ReadFile(keyFile)
			if err != nil {
				return "", fmt.Errorf("read the model group's key file: %w", err)
			}
			return strings.TrimSpace(string(key)), nil
		}
	}
	endpoint := agentloop.Endpoint{BaseURL: strings.TrimRight(g.Endpoint, "/"), Token: token}
	effort := "off"
	switch g.Kind {
	case modelKindAzure:
		endpoint.Kind = agentloop.OpenAIChat
		if g.APIVersion != "" {
			endpoint.Query = url.Values{"api-version": {g.APIVersion}}
		}
		if writingcheck.ReaderNoEffort {
			effort = ""
		}
	case modelKindAnthropic:
		endpoint.Kind = agentloop.AnthropicMessages
	default:
		return agentloop.Model{}, fmt.Errorf("a model group of kind %q calls no model", g.Kind)
	}
	if g.Effort != "" {
		effort = g.Effort
	}
	return agentloop.Model{Endpoint: endpoint, ID: g.Deployment, Effort: effort}, nil
}

// share is what the group asks of its deployment on the pacer: the deployment as the pacer keys
// it (pacedDeployment), with the group's tokens-per-minute setting the ceiling on its share. The
// boot weighs this same share against the least the group's calls are charged
// (refuseQuotaNoCallFits).
func (g modelGroup) share() (modelpace.Share, error) {
	deployment, err := pacedDeployment(g.Kind, g.Endpoint, g.Deployment)
	if err != nil {
		return modelpace.Share{}, fmt.Errorf("the model group's endpoint %q is not a URL", g.Endpoint)
	}
	return modelpace.Share{Deployment: deployment, TokensPerMinute: g.TokensPerMinute}, nil
}

// pace is the group's share of its deployment on pacer, the process's one pacer (modelpace): its
// tokens-per-minute setting is the ceiling on that share.
func (g modelGroup) pace(pacer *modelpace.Pacer) (*modelpace.Caller, error) {
	share, err := g.share()
	if err != nil {
		return nil, err
	}
	return pacer.Join(share), nil
}

// resolveWritingSettings reads the settings of the work on agents' texts and refuses, naming the
// setting, every value the work could not run on: a choice outside its values, a rule set no
// reviewer applies, a duration or a time that does not parse, a malformed quota, a reviewer that
// reviews with no model group, a model group set in part (resolveModelGroup), a minute's quota no
// call of the calls it paces fits in (refuseQuotaNoCallFits), and DISPATCH_TEST_MODEL_URL and
// DISPATCH_TEST_MODELS without the test hooks. Each setting left unset is the default the api and
// reviewer packages export, the one place each default is written. answerer is the answerer the
// boot resolved (resolveAnswerer), whose quota paces a group on its deployment.
func resolveWritingSettings(env settingValues, testHooksEnabled bool, answerer answererConfig) (writingSettings, error) {
	var cfg writingSettings
	if err := testHookURL(env, "DISPATCH_TEST_MODEL_URL", testHooksEnabled, &cfg.TestModelURL); err != nil {
		return writingSettings{}, err
	}
	var err error
	if cfg.TestModels, err = resolveTestModels(env, testHooksEnabled, cfg.TestModelURL); err != nil {
		return writingSettings{}, err
	}
	for _, spec := range modelGroupSpecs {
		if *spec.group(&cfg), err = resolveModelGroup(env, spec.prefix, spec.whose, cfg.TestModelURL); err != nil {
			return writingSettings{}, err
		}
	}
	holdable := []string{api.ReviewOff, api.ReviewAfter, api.ReviewHold}
	for _, row := range []struct {
		name    string
		target  *string
		choices []string
	}{
		{"DISPATCH_REVIEWER_ASKS", &cfg.ReviewAsks, holdable},
		{"DISPATCH_REVIEWER_ISSUES", &cfg.ReviewIssues, holdable},
		{"DISPATCH_REVIEWER_COMMENTS", &cfg.ReviewComments, holdable},
		{"DISPATCH_REVIEWER_MESSAGES", &cfg.ReviewMessages, holdable},
		{"DISPATCH_REVIEWER_DOCUMENTS", &cfg.ReviewDocuments, []string{api.ReviewOff, api.ReviewAfter}},
		{"DISPATCH_REVIEWER_DECISION_BLOCKS", &cfg.ReviewDecisionBlocks, holdable},
	} {
		choice, err := choiceSetting(env, row.name, row.choices, api.ReviewOff)
		if err != nil {
			return writingSettings{}, err
		}
		if choice != api.ReviewOff && cfg.ReviewerModel.Kind == "" {
			return writingSettings{}, fmt.Errorf("%s=%s needs the reviewer's model group (%sKIND and the settings beside it): with no model the reviewer reads nothing", row.name, choice, reviewerModelSpec.prefix)
		}
		*row.target = choice
	}
	if cfg.ReviewerRules, err = listSetting(env, "DISPATCH_REVIEWER_RULES", reviewer.AllRuleSets, reviewer.DefaultRules); err != nil {
		return writingSettings{}, err
	}
	if cfg.ReviewerTimeout, err = durationSetting(env, "DISPATCH_REVIEWER_TIMEOUT", reviewer.DefaultTimeout); err != nil {
		return writingSettings{}, err
	}
	if cfg.ReviewerWriterWindow, err = durationSetting(env, "DISPATCH_REVIEWER_WRITER_WINDOW", api.DefaultWriterWindow); err != nil {
		return writingSettings{}, err
	}
	if cfg.ReviewerUnedited, err = choiceSetting(env, "DISPATCH_REVIEWER_UNEDITED", []string{api.UneditedRewrite, api.UneditedAsWritten}, api.UneditedRewrite); err != nil {
		return writingSettings{}, err
	}
	if cfg.WordRecords, err = onOffSetting(env, "DISPATCH_WORD_RECORDS", true); err != nil {
		return writingSettings{}, err
	}
	from := cmp.Or(strings.TrimSpace(env.get("DISPATCH_WORD_RECORDS_FROM")), defaultWordRecordsFrom)
	if cfg.WordRecordsFrom, err = time.Parse(time.RFC3339, from); err != nil {
		return writingSettings{}, fmt.Errorf("DISPATCH_WORD_RECORDS_FROM=%q (expected an RFC 3339 time, such as %s)", from, defaultWordRecordsFrom)
	}
	if cfg.WritingRulesRefusal, err = onOffSetting(env, "DISPATCH_WRITING_RULES_REFUSAL", false); err != nil {
		return writingSettings{}, err
	}
	if cfg.WritingRulesExtra, err = listSetting(env, "DISPATCH_WRITING_RULES_EXTRA", writingRulesExtraSets, nil); err != nil {
		return writingSettings{}, err
	}
	if cfg.DeploymentQuotas, err = deploymentQuotas(env); err != nil {
		return writingSettings{}, err
	}
	if err := refuseQuotaNoCallFits(cfg, answerer); err != nil {
		return writingSettings{}, err
	}
	return cfg, nil
}

// refuseQuotaNoCallFits refuses a minute's quota that could serve no call of a set model group or
// of a test model: one at or under the least any of the calls the group sends is charged, its
// reply bound and the estimated tokens of the shortest system prompt of those calls under
// DISPATCH_REVIEWER_RULES (modelGroupSpec.leastCharge, modelpace.Charge, the charge the pacer
// takes): the reviewer's group's reviews, Rewrite and Define's rewrites and definitions, and the
// records job's calls. The pacer charges a call its prompt, the system prompt and the user message
// together, and its whole reply bound before the call is sent, and refuses for good a call that
// costs more than a minute's whole quota; every call's user message holds some of the text. Under
// such a quota every call would fail with nothing at boot saying why, and each reviewed text would
// post as written. Three settings can be that quota, and each is weighed as the pacer reads it,
// from the shares main joins the pacer with (modelGroup.share, answererConfig.share):
//
//   - the group's own _TOKENS_PER_MINUTE ceiling;
//   - the quota DISPATCH_MODEL_DEPLOYMENT_QUOTAS names for the group's deployment, which is then
//     the deployment's rate;
//   - where it names none, the quota answerer states, if the group's deployment is the
//     answerer's as the pacer keys it (pacedDeployment). An answerer of another kind, on another
//     endpoint host or of another deployment is another limiter, and is not weighed.
//
// A test model serves the reviewer's reviews and Rewrite and Define's rewrites and definitions, so
// it is held to the shortest of them. A quota for a deployment that no group and no test model
// names is not weighed.
func refuseQuotaNoCallFits(cfg writingSettings, answerer answererConfig) error {
	const why = "each call is charged its prompt's estimated tokens and its whole reply bound before it is sent, so a minute's quota no larger could serve no call"
	// stated is what the answerer asks of its deployment. A share of no tokens states no quota,
	// as the pacer reads it: the endpoint form's, and that of no answerer.
	stated, err := answerer.share()
	if err != nil {
		return err
	}
	// refuse weighs a set group's ceiling, then the quota that paces its deployment, against the
	// least its calls are charged. ceiling and deployment are the settings that name the group's
	// two.
	refuse := func(spec modelGroupSpec, group modelGroup, ceiling, deployment string) error {
		if group.Kind == "" {
			return nil
		}
		asked, err := group.share()
		if err != nil {
			return err
		}
		least, err := spec.leastCharge(cfg.ReviewerRules)
		if err != nil {
			return err
		}
		charged := fmt.Sprintf("%d, the least %s %s are charged (a reply bound of %d tokens and the shortest system prompt of those calls)", least, spec.whose, spec.calls, spec.replyBound)
		if asked.TokensPerMinute <= least {
			return fmt.Errorf("%s=%d is not more than %s: %s", ceiling, asked.TokensPerMinute, charged, why)
		}
		if quota, named := cfg.DeploymentQuotas[asked.Deployment.Name]; named {
			if quota <= least {
				return fmt.Errorf("%s names %s=%d, not more than %s, which go to that deployment (%s): %s",
					deploymentQuotasSetting, asked.Deployment.Name, quota, charged, deployment, why)
			}
			return nil
		}
		if stated.StatesQuota && stated.Deployment == asked.Deployment && 0 < stated.TokensPerMinute && stated.TokensPerMinute <= least {
			return fmt.Errorf("%s=%d is not more than %s, which go to the answerer's deployment (%s) and are paced at its quota, since %s names none for it: %s",
				answererQuotaSetting, stated.TokensPerMinute, charged, deployment, deploymentQuotasSetting, why)
		}
		return nil
	}
	for _, spec := range modelGroupSpecs {
		if err := refuse(spec, *spec.group(&cfg), spec.prefix+"TOKENS_PER_MINUTE", spec.prefix+"DEPLOYMENT"); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.TestModels)) {
		group := cfg.TestModels[name]
		spec := testModelSpec(group.Kind)
		if err := refuse(spec, group, spec.prefix+"TOKENS_PER_MINUTE", testModelsSetting); err != nil {
			return err
		}
	}
	return nil
}

// answererQuotaSetting states the quota of the answerer's deployment under DISPATCH_ANSWERER=azure.
const answererQuotaSetting = azureChatSettingPrefix + "TOKENS_PER_MINUTE"

// modelGroupsReadSignIn reports whether a model group reads the shared Azure sign-in beside an
// azure embedder and an azure answerer: a set group whose kind is not anthropic, or a test model
// of DISPATCH_TEST_MODELS whose kind is not. An azure group reads it, and a group whose kind is
// missing or unknown is counted too, so resolveModelGroup's refusal of that kind is the one an
// operator reads, not a refusal of the sign-in beside it.
func modelGroupsReadSignIn(env settingValues) bool {
	for _, spec := range modelGroupSpecs {
		if len(settingsSet(env, spec.prefix)) > 0 && strings.TrimSpace(env.get(spec.prefix+"KIND")) != modelKindAnthropic {
			return true
		}
	}
	for _, entry := range strings.Split(env.get(testModelsSetting), ",") {
		if _, kind, found := strings.Cut(entry, "="); found && strings.TrimSpace(kind) != modelKindAnthropic {
			return true
		}
	}
	return false
}

// modelKinds is what each kind of model group reads: the schemes its endpoint may have, the route
// its client appends to the endpoint, and the settings of a group it takes no value from, with
// why.
var modelKinds = map[string]struct {
	schemes []string
	route   string
	notRead []string
	why     string
}{
	modelKindAzure: {schemes: []string{"https"}, route: "/chat/completions", notRead: []string{"KEY_FILE"},
		why: "an azure group signs in with the shared Azure sign-in and presents no key"},
	// The model gateway serves plain HTTP inside the cluster: http is taken for a cluster-local
	// host alone (clusterLocalHost).
	modelKindAnthropic: {schemes: []string{"http", "https"}, route: "/v1/messages", notRead: []string{"API_VERSION", "SCOPE"},
		why: "an anthropic group sends no api-version and asks for no Azure token"},
}

// clusterLocalHost reports whether host is reached inside the machine or the cluster: localhost, a
// loopback address, or a Kubernetes Service's name (ending .svc or .cluster.local). A call over
// plain http goes to such a host alone, since each call of a model group carries its key (OWASP
// ASVS 5.0 12.3.1).
func clusterLocalHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost" || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".cluster.local")
}

// resolveModelGroup reads the model group whose settings begin with prefix. A group none of whose
// settings is set is unset, which is no model. A set group is refused, naming the settings, when
// its kind is neither azure nor anthropic; when it is missing a setting it needs (its kind, its
// endpoint, its deployment, its tokens-per-minute ceiling, and an anthropic group's key file);
// when it sets one its kind does not read (an azure group's key file, an anthropic group's
// api-version or scope); when its endpoint or its ceiling is malformed, or its endpoint is http
// on a host outside the cluster (clusterLocalHost); and when an azure group finds the shared
// Azure sign-in unset or set in part. Under testModelURL, the stand-in takes the place of the
// group's endpoint and credential: the group needs neither, refuses its endpoint, key file and
// scope, and reads no sign-in.
func resolveModelGroup(env settingValues, prefix, whose, testModelURL string) (modelGroup, error) {
	set := settingsSet(env, prefix)
	if len(set) == 0 {
		return modelGroup{}, nil
	}
	value := func(field string) string { return strings.TrimSpace(env.get(prefix + field)) }
	group := modelGroup{Kind: value("KIND"), Deployment: value("DEPLOYMENT"), APIVersion: value("API_VERSION")}
	kind, known := modelKinds[group.Kind]
	if group.Kind != "" && !known {
		return modelGroup{}, fmt.Errorf("%sKIND=%q (expected azure or anthropic)", prefix, group.Kind)
	}
	needed := []string{"KIND", "ENDPOINT", "DEPLOYMENT", "TOKENS_PER_MINUTE"}
	switch {
	case testModelURL != "":
		needed = slices.DeleteFunc(needed, func(field string) bool { return field == "ENDPOINT" })
	case group.Kind == modelKindAnthropic:
		needed = append(needed, "KEY_FILE")
	}
	var missing []string
	for _, field := range needed {
		if value(field) == "" {
			missing = append(missing, prefix+field)
		}
	}
	if len(missing) > 0 {
		return modelGroup{}, fmt.Errorf("%s set, but %s missing: %s model group is set whole or not at all", strings.Join(set, ", "), strings.Join(missing, ", "), whose)
	}
	for _, field := range kind.notRead {
		if value(field) != "" {
			return modelGroup{}, fmt.Errorf("%s%s set, but %sKIND is %s: %s", prefix, field, prefix, group.Kind, kind.why)
		}
	}
	tokens, err := positiveSetting(env, prefix+"TOKENS_PER_MINUTE")
	if err != nil {
		return modelGroup{}, err
	}
	group.TokensPerMinute = tokens
	if testModelURL != "" {
		for _, field := range []string{"ENDPOINT", "KEY_FILE", "SCOPE"} {
			if value(field) != "" {
				return modelGroup{}, fmt.Errorf("%s%s set beside DISPATCH_TEST_MODEL_URL: the stand-in model takes the place of the group's endpoint and credential, so set one or the other", prefix, field)
			}
		}
		group.Endpoint, group.StandIn = testModelURL, true
		return group, nil
	}
	group.Endpoint = value("ENDPOINT")
	parsed, err := url.Parse(group.Endpoint)
	if err != nil || !slices.Contains(kind.schemes, parsed.Scheme) || parsed.Host == "" || strings.ContainsAny(group.Endpoint, "?#") {
		return modelGroup{}, fmt.Errorf("%sENDPOINT=%q (expected an absolute %s URL with a host and no query or fragment, the base URL %s is appended to)",
			prefix, group.Endpoint, strings.Join(kind.schemes, " or "), kind.route)
	}
	if parsed.Scheme == "http" && !clusterLocalHost(parsed.Hostname()) {
		return modelGroup{}, fmt.Errorf("%sENDPOINT=%q is http to a host outside the cluster, which would carry %s model group's key in clear text: use https, or http to localhost, a loopback address or a cluster Service (*.svc, *.cluster.local)",
			prefix, group.Endpoint, whose)
	}
	if group.Kind == modelKindAnthropic {
		group.KeyFile = value("KEY_FILE")
		return group, nil
	}
	group.Scope = cmp.Or(value("SCOPE"), embed.DefaultEntraScope)
	signIn := readAzureSignIn(env)
	group.TenantID, group.ClientID, group.FederatedToken = signIn.TenantID, signIn.ClientID, signIn.TokenFile
	switch {
	case len(signIn.set) == 0:
		return modelGroup{}, fmt.Errorf("%sKIND=azure needs the keyless Azure sign-in (%s): %s model group takes no API key", prefix, azureKeylessNames, whose)
	case len(signIn.missing) > 0:
		return modelGroup{}, signIn.refusePartial()
	}
	return group, nil
}

// testModelsSetting lists the models a test request may name in its Dispatch-Test-Model header.
const testModelsSetting = "DISPATCH_TEST_MODELS"

// testModelPrefixes are the prefixes of the settings of the test models of each kind
// (DISPATCH_TEST_MODELS_<KIND>_*): where a test model of the kind is called, with what credential,
// and the ceiling of its share of its deployment.
var testModelPrefixes = map[string]string{
	modelKindAzure:     "DISPATCH_TEST_MODELS_AZURE_",
	modelKindAnthropic: "DISPATCH_TEST_MODELS_ANTHROPIC_",
}

// testModelsWhose is whose group the test models of kind are, as a refusal names it.
func testModelsWhose(kind string) string {
	return "the " + kind + " test models'"
}

// testModelEfforts are the reasoning efforts a test model's name may ask for: the words agentloop
// reads on each protocol it speaks. off asks for no reasoning (OpenAI's protocols send it as
// `none`, Anthropic's turn thinking off), minimal is Anthropic's low, and low, medium and high are
// each provider's own.
var testModelEfforts = []string{"off", "minimal", "low", "medium", "high"}

// resolveTestModels reads DISPATCH_TEST_MODELS: a comma list of name=kind, each name once, kind
// azure or anthropic, refused without the test hooks. A name is a model's id, or `<id>:<effort>`
// with effort one of testModelEfforts, which asks that model for that effort; an effort not in
// that list is refused, naming it. The test models of one kind are that kind's
// DISPATCH_TEST_MODELS_<KIND>_* settings read as a model group of the kind (resolveTestModelKind),
// each with its name's id as the deployment its requests name. The settings of a kind that no
// listed model has are refused. Unset, it is nil.
func resolveTestModels(env settingValues, testHooksEnabled bool, testModelURL string) (map[string]modelGroup, error) {
	raw := strings.TrimSpace(env.get(testModelsSetting))
	if raw != "" && !testHooksEnabled {
		return nil, fmt.Errorf("DISPATCH_TEST_HOOKS=1 required with %s", testModelsSetting)
	}
	models := map[string]modelGroup{}
	kinds := map[string]modelGroup{}
	for _, entry := range strings.Split(raw, ",") {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		name, kind, found := strings.Cut(entry, "=")
		name, kind = strings.TrimSpace(name), strings.TrimSpace(kind)
		id, effort, named := strings.Cut(name, ":")
		prefix, known := testModelPrefixes[kind]
		if !found || id == "" || !known {
			return nil, fmt.Errorf("%s names %q (expected a comma list of name=kind, each a model's name and its kind, azure or anthropic)", testModelsSetting, entry)
		}
		if named && !slices.Contains(testModelEfforts, effort) {
			return nil, fmt.Errorf("%s names %s, whose effort %q is not one agentloop takes (expected <id> or <id>:<effort>, the effort one of %s)",
				testModelsSetting, name, effort, strings.Join(testModelEfforts, ", "))
		}
		if _, twice := models[name]; twice {
			return nil, fmt.Errorf("%s names %s twice", testModelsSetting, name)
		}
		group, resolved := kinds[kind]
		if !resolved {
			var err error
			if group, err = resolveTestModelKind(env, prefix, kind, testModelURL); err != nil {
				return nil, err
			}
			kinds[kind] = group
		}
		group.Deployment, group.Effort = id, effort
		models[name] = group
	}
	for kind, prefix := range testModelPrefixes {
		if _, listed := kinds[kind]; !listed {
			if set := settingsSet(env, prefix); len(set) > 0 {
				return nil, fmt.Errorf("%s set, but %s names no %s model", strings.Join(set, ", "), testModelsSetting, kind)
			}
		}
	}
	if len(models) == 0 {
		return nil, nil
	}
	return models, nil
}

// resolveTestModelKind reads the settings of the test models of kind under prefix as
// resolveModelGroup reads a model group of that kind, so it refuses each shape a group is refused
// for: its endpoint, an anthropic model's key file and the ceiling are needed, and beside the
// stand-in model (testModelURL) the ceiling alone, every call then going to the stand-in. A kind
// has no row for a field its group would not read (an azure key file, an anthropic api-version or
// scope), so each such field reads as unset.
func resolveTestModelKind(env settingValues, prefix, kind, testModelURL string) (modelGroup, error) {
	whose := testModelsWhose(kind)
	if len(settingsSet(env, prefix)) == 0 {
		return modelGroup{}, fmt.Errorf("%s names an %s model, but no %s* setting is set: %s group is set whole or not at all", testModelsSetting, kind, prefix, whose)
	}
	asGroup := maps.Clone(env)
	for _, field := range []string{"ENDPOINT", "API_VERSION", "SCOPE", "KEY_FILE", "TOKENS_PER_MINUTE"} {
		if _, listed := asGroup[prefix+field]; !listed {
			asGroup[prefix+field] = settingValue{}
		}
	}
	asGroup[prefix+"KIND"] = settingValue{value: kind, set: true}
	asGroup[prefix+"DEPLOYMENT"] = settingValue{value: testModelsSetting, set: true}
	return resolveModelGroup(asGroup, prefix, whose, testModelURL)
}

// deploymentQuotasSetting gives each deployment's tokens-per-minute quota, by the deployment's
// name.
const deploymentQuotasSetting = "DISPATCH_MODEL_DEPLOYMENT_QUOTAS"

// deploymentQuotas reads DISPATCH_MODEL_DEPLOYMENT_QUOTAS: a comma list of name=tokens_per_minute,
// each name once, each quota a positive integer. Unset, it is nil.
func deploymentQuotas(env settingValues) (map[string]int, error) {
	raw := strings.TrimSpace(env.get(deploymentQuotasSetting))
	if raw == "" {
		return nil, nil
	}
	quotas := map[string]int{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		deployment, tokens, found := strings.Cut(entry, "=")
		deployment, tokens = strings.TrimSpace(deployment), strings.TrimSpace(tokens)
		quota, err := strconv.Atoi(tokens)
		if !found || deployment == "" || err != nil || strings.TrimLeft(tokens, "0123456789") != "" || quota < 1 {
			return nil, fmt.Errorf("%s names %q (expected a comma list of name=tokens_per_minute, each a deployment's name and its quota, a positive integer)", deploymentQuotasSetting, entry)
		}
		if _, twice := quotas[deployment]; twice {
			return nil, fmt.Errorf("%s names %s twice", deploymentQuotasSetting, deployment)
		}
		quotas[deployment] = quota
	}
	return quotas, nil
}

// onOffSetting reads a setting that is on or off, unset or empty being fallback.
func onOffSetting(env settingValues, name string, fallback bool) (bool, error) {
	switch value := strings.TrimSpace(env.get(name)); value {
	case "":
		return fallback, nil
	case "on":
		return true, nil
	case "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s=%q (expected on or off)", name, value)
	}
}

// choiceSetting reads a setting that is one of choices, unset or empty being fallback.
func choiceSetting(env settingValues, name string, choices []string, fallback string) (string, error) {
	value := strings.TrimSpace(env.get(name))
	switch {
	case value == "":
		return fallback, nil
	case slices.Contains(choices, value):
		return value, nil
	}
	return "", fmt.Errorf("%s=%q (expected %s or %s)", name, value, strings.Join(choices[:len(choices)-1], ", "), choices[len(choices)-1])
}

// listSetting reads a comma list of rule sets from known, each once in the order given; unset or
// empty, it is fallback.
func listSetting(env settingValues, name string, known, fallback []reviewer.RuleSet) ([]reviewer.RuleSet, error) {
	raw := strings.TrimSpace(env.get(name))
	if raw == "" {
		return slices.Clone(fallback), nil
	}
	list := []reviewer.RuleSet{}
	for _, entry := range strings.Split(raw, ",") {
		item := reviewer.RuleSet(strings.TrimSpace(entry))
		if item == "" || slices.Contains(list, item) {
			continue
		}
		if !slices.Contains(known, item) {
			return nil, fmt.Errorf("%s names %q, which is not one of %s", name, item, strings.Join(ruleSetNames(known...), ", "))
		}
		list = append(list, item)
	}
	return list, nil
}

// durationSetting reads a positive Go duration, unset or empty being fallback.
func durationSetting(env settingValues, name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(env.get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s=%q (expected a positive Go duration)", name, raw)
	}
	return value, nil
}
