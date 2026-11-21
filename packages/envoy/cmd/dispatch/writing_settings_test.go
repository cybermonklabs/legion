package main

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/modelpace"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
	"github.com/sjawhar/envoy/internal/dispatch/wordrecords"
)

const (
	reviewerModel      = "DISPATCH_REVIEWER_MODEL_"
	rewriteDefineModel = "DISPATCH_REWRITE_DEFINE_MODEL_"
	wordRecordsModel   = "DISPATCH_WORD_RECORDS_MODEL_"
	// testModelsPrefix stands for the test models of either kind (DISPATCH_TEST_MODELS) where a
	// helper takes a group's prefix.
	testModelsPrefix = "DISPATCH_TEST_MODELS_"
	// gatewayEndpoint is the model gateway as a pod reaches it, over plain HTTP to a cluster
	// Service.
	gatewayEndpoint = "http://model-gateway.model-gateway.svc.cluster.local:4000"
)

// groupOf is each model group of a resolved boot, by its settings' prefix.
var groupOf = map[string]func(writingSettings) modelGroup{
	reviewerModel:      func(w writingSettings) modelGroup { return w.ReviewerModel },
	rewriteDefineModel: func(w writingSettings) modelGroup { return w.RewriteDefineModel },
	wordRecordsModel:   func(w writingSettings) modelGroup { return w.WordRecordsModel },
}

// azureGroup is a whole azure model group under prefix, with the shared Azure sign-in beside it,
// which the group reads with no azure embedder or answerer.
func azureGroup(prefix string, overrides map[string]string) map[string]string {
	return withOverrides(withOverrides(map[string]string{
		prefix + "KIND":              "azure",
		prefix + "ENDPOINT":          "https://models.example/models",
		prefix + "DEPLOYMENT":        "grok-4.7",
		prefix + "TOKENS_PER_MINUTE": "60000",
	}, azureFederated), overrides)
}

// anthropicGroup is a whole anthropic model group under prefix, at a gateway over plain HTTP.
func anthropicGroup(prefix string, overrides map[string]string) map[string]string {
	return withOverrides(map[string]string{
		prefix + "KIND":              "anthropic",
		prefix + "ENDPOINT":          gatewayEndpoint,
		prefix + "DEPLOYMENT":        "claude-haiku-4-5-20251001",
		prefix + "KEY_FILE":          "/var/run/model-gateway-key/key",
		prefix + "TOKENS_PER_MINUTE": "40000",
	}, overrides)
}

// standInGroup is a model group under prefix as a test process sets it beside the stand-in model:
// its kind, its deployment and its ceiling, under the test hooks.
func standInGroup(prefix, kind string, overrides map[string]string) map[string]string {
	return withOverrides(map[string]string{
		"DISPATCH_TEST_HOOKS":        "1",
		"DISPATCH_TEST_MODEL_URL":    "http://127.0.0.1:9040",
		prefix + "KIND":              kind,
		prefix + "DEPLOYMENT":        "stand-in-" + strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(prefix, "DISPATCH_"), "_MODEL_")),
		prefix + "TOKENS_PER_MINUTE": "1000000",
	}, overrides)
}

// Every default keeps each change to a text off: no kind is reviewed, the written rules refuse
// nothing, no model group is set, so neither Rewrite and Define nor the records job runs, and
// the deployments have no quota of their own.
func TestEveryWritingDefaultKeepsEachChangeOff(t *testing.T) {
	writing := resolveWith(t, nil).Writing
	for name, choice := range map[string]string{
		"DISPATCH_REVIEWER_ASKS": writing.ReviewAsks, "DISPATCH_REVIEWER_ISSUES": writing.ReviewIssues,
		"DISPATCH_REVIEWER_COMMENTS": writing.ReviewComments, "DISPATCH_REVIEWER_MESSAGES": writing.ReviewMessages,
		"DISPATCH_REVIEWER_DOCUMENTS": writing.ReviewDocuments, "DISPATCH_REVIEWER_DECISION_BLOCKS": writing.ReviewDecisionBlocks,
	} {
		if choice != api.ReviewOff {
			t.Errorf("%s = %q, want off", name, choice)
		}
	}
	if writing.WritingRulesRefusal || len(writing.WritingRulesExtra) != 0 {
		t.Errorf("the written rules: refusal %t, extra %v, want off and none", writing.WritingRulesRefusal, writing.WritingRulesExtra)
	}
	for prefix, group := range groupOf {
		if got := group(writing); got != (modelGroup{}) {
			t.Errorf("%s group = %+v, want unset", prefix, got)
		}
	}
	if writing.DeploymentQuotas != nil || writing.TestModelURL != "" {
		t.Errorf("quotas %v, stand-in %q, want neither", writing.DeploymentQuotas, writing.TestModelURL)
	}
}

// A whole group of either kind parses into the group the server's model clients are built from.
func TestAWholeModelGroupParses(t *testing.T) {
	for prefix, group := range groupOf {
		t.Run(prefix, func(t *testing.T) {
			if got, want := group(resolveWith(t, azureGroup(prefix, nil)).Writing), (modelGroup{
				Kind: "azure", Endpoint: "https://models.example/models", Deployment: "grok-4.7",
				Scope: embed.DefaultEntraScope, TokensPerMinute: 60000,
				TenantID: "tenant-id", ClientID: "client-id", FederatedToken: "/var/run/secrets/azure-federation/token",
			}); got != want {
				t.Errorf("azure group = %+v, want %+v", got, want)
			}
			if got, want := group(resolveWith(t, anthropicGroup(prefix, nil)).Writing), (modelGroup{
				Kind: "anthropic", Endpoint: gatewayEndpoint, Deployment: "claude-haiku-4-5-20251001",
				KeyFile: "/var/run/model-gateway-key/key", TokensPerMinute: 40000,
			}); got != want {
				t.Errorf("anthropic group = %+v, want %+v", got, want)
			}
		})
	}
}

// A group set in part refuses the boot, naming each setting it is missing: its kind, its endpoint,
// its deployment and its ceiling, and an anthropic group's key file.
func TestAModelGroupSetInPartRefusesBootNamingTheMissingSetting(t *testing.T) {
	for prefix := range groupOf {
		t.Run(prefix, func(t *testing.T) {
			for _, field := range []string{"KIND", "ENDPOINT", "DEPLOYMENT", "TOKENS_PER_MINUTE"} {
				refusedWith(t, azureGroup(prefix, map[string]string{prefix + field: ""}), prefix+field+" missing")
				refusedWith(t, anthropicGroup(prefix, map[string]string{prefix + field: " "}), prefix+field+" missing")
			}
			refusedWith(t, anthropicGroup(prefix, map[string]string{prefix + "KEY_FILE": ""}), prefix+"KEY_FILE missing")
			// One setting alone names every one the group still needs.
			refusedWith(t, map[string]string{prefix + "DEPLOYMENT": "grok-4.7"},
				prefix+"DEPLOYMENT set, but "+prefix+"KIND, "+prefix+"ENDPOINT, "+prefix+"TOKENS_PER_MINUTE missing")
		})
	}
}

// DISPATCH_TEST_MODEL_URL is a test process's alone: without DISPATCH_TEST_HOOKS=1 it refuses the
// boot. Under the hooks every set group's calls go to it, each naming the group's deployment,
// with no credential: the group needs no endpoint, key file or sign-in, and refuses them.
func TestTheStandInModelURLNeedsTheTestHooks(t *testing.T) {
	refusedWith(t, map[string]string{"DISPATCH_TEST_MODEL_URL": "http://127.0.0.1:9040"}, "DISPATCH_TEST_HOOKS=1 required with DISPATCH_TEST_MODEL_URL")
	refusedWith(t, standInGroup(reviewerModel, "anthropic", map[string]string{"DISPATCH_TEST_HOOKS": ""}), "DISPATCH_TEST_HOOKS=1 required with DISPATCH_TEST_MODEL_URL")
	env := withOverrides(standInGroup(reviewerModel, "anthropic", nil), standInGroup(wordRecordsModel, "azure", map[string]string{wordRecordsModel + "API_VERSION": "2024-05-01-preview"}))
	writing := resolveWith(t, env).Writing
	if got, want := writing.ReviewerModel, (modelGroup{Kind: "anthropic", Endpoint: "http://127.0.0.1:9040", Deployment: "stand-in-reviewer", TokensPerMinute: 1000000, StandIn: true}); got != want {
		t.Errorf("reviewer's group = %+v, want %+v", got, want)
	}
	if got, want := writing.WordRecordsModel, (modelGroup{Kind: "azure", Endpoint: "http://127.0.0.1:9040", Deployment: "stand-in-word_records", APIVersion: "2024-05-01-preview", TokensPerMinute: 1000000, StandIn: true}); got != want {
		t.Errorf("records job's group = %+v, want %+v", got, want)
	}
	if writing.RewriteDefineModel != (modelGroup{}) {
		t.Errorf("Rewrite and Define's group = %+v, want unset: the stand-in sets no group", writing.RewriteDefineModel)
	}
	for _, field := range []string{"ENDPOINT", "KEY_FILE", "SCOPE"} {
		refusedWith(t, standInGroup(rewriteDefineModel, "azure", map[string]string{rewriteDefineModel + field: "x"}), rewriteDefineModel+field)
	}
}

// testModelsEnv is a test process's DISPATCH_TEST_MODELS with one model of each kind, each kind's
// settings, and the shared Azure sign-in the azure model reads.
func testModelsEnv(overrides map[string]string) map[string]string {
	return withOverrides(withOverrides(map[string]string{
		"DISPATCH_TEST_HOOKS":                              "1",
		"DISPATCH_TEST_MODELS":                             " grok-4.7=azure , claude-haiku-5-5 = anthropic,",
		"DISPATCH_TEST_MODELS_AZURE_ENDPOINT":              "https://models.example/models",
		"DISPATCH_TEST_MODELS_AZURE_TOKENS_PER_MINUTE":     "60000",
		"DISPATCH_TEST_MODELS_ANTHROPIC_ENDPOINT":          gatewayEndpoint,
		"DISPATCH_TEST_MODELS_ANTHROPIC_KEY_FILE":          "/var/run/model-gateway-key/key",
		"DISPATCH_TEST_MODELS_ANTHROPIC_TOKENS_PER_MINUTE": "40000",
	}, azureFederated), overrides)
}

// noAzureSignIn unsets the shared Azure sign-in, for a test process that lists no azure model, so
// the refusal of a sign-in no one reads does not come first.
var noAzureSignIn = map[string]string{"DISPATCH_AZURE_TENANT_ID": "", "DISPATCH_AZURE_CLIENT_ID": "", "DISPATCH_AZURE_FEDERATED_TOKEN_FILE": ""}

// An anthropic group asks for no thinking, and an azure group, a test model of either kind
// included, sends no reasoning effort at all, since a Foundry chat deployment refuses `none`.
func TestAnAzureGroupSendsNoReasoningEffort(t *testing.T) {
	models := resolveWith(t, testModelsEnv(nil)).Writing.TestModels
	for name, want := range map[string]string{"grok-4.7": "", "claude-haiku-5-5": "off"} {
		model, err := models[name].agentloopModel()
		if err != nil {
			t.Fatal(err)
		}
		if model.Effort != want || model.ID != name {
			t.Errorf("%s calls %q at effort %q, want %q", name, model.ID, model.Effort, want)
		}
	}
}

// A test model's name may ask for a reasoning effort, `<id>:<effort>`: that model calls deployment
// <id> at that effort, of either kind, beside the same id named alone, which asks for its kind's
// own as before. An effort agentloop does not take refuses the boot, naming it, and so does a
// name with no id.
func TestATestModelNamedWithAnEffortAsksForIt(t *testing.T) {
	models := resolveWith(t, testModelsEnv(map[string]string{
		"DISPATCH_TEST_MODELS": "Kimi-K3.1-Pro=azure,Kimi-K3.1-Pro:off=azure,claude-haiku-5-5:high=anthropic",
	})).Writing.TestModels
	plain := modelGroup{Kind: "azure", Endpoint: "https://models.example/models", Deployment: "Kimi-K3.1-Pro", Scope: embed.DefaultEntraScope,
		TokensPerMinute: 60000, TenantID: "tenant-id", ClientID: "client-id", FederatedToken: "/var/run/secrets/azure-federation/token"}
	off := plain
	off.Effort = "off"
	high := modelGroup{Kind: "anthropic", Endpoint: gatewayEndpoint, Deployment: "claude-haiku-5-5",
		KeyFile: "/var/run/model-gateway-key/key", TokensPerMinute: 40000, Effort: "high"}
	if want := map[string]modelGroup{"Kimi-K3.1-Pro": plain, "Kimi-K3.1-Pro:off": off, "claude-haiku-5-5:high": high}; !maps.Equal(models, want) {
		t.Errorf("TestModels = %+v, want %+v", models, want)
	}
	for name, want := range map[string]struct{ id, effort string }{
		"Kimi-K3.1-Pro":         {"Kimi-K3.1-Pro", ""},
		"Kimi-K3.1-Pro:off":     {"Kimi-K3.1-Pro", "off"},
		"claude-haiku-5-5:high": {"claude-haiku-5-5", "high"},
	} {
		model, err := models[name].agentloopModel()
		if err != nil {
			t.Fatal(err)
		}
		if model.ID != want.id || model.Effort != want.effort {
			t.Errorf("%s calls %q at effort %q, want %q at %q", name, model.ID, model.Effort, want.id, want.effort)
		}
	}

	for name, effort := range map[string]string{"Kimi-K3.1-Pro:max": "max", "Kimi-K3.1-Pro:": "", "Kimi-K3.1-Pro:off:low": "off:low", "Kimi-K3.1-Pro:OFF": "OFF"} {
		refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS": name + "=azure,claude-haiku-5-5=anthropic"}),
			"DISPATCH_TEST_MODELS names "+name+", whose effort "+strconv.Quote(effort)+" is not one agentloop takes (expected <id> or <id>:<effort>, the effort one of off, minimal, low, medium, high)")
	}
	refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS": ":off=azure,claude-haiku-5-5=anthropic"}), `DISPATCH_TEST_MODELS names ":off=azure"`)
}

// DISPATCH_TEST_MODELS is a test process's alone: without DISPATCH_TEST_HOOKS=1 it refuses the
// boot. Under the hooks each listed model is its kind's group, named as its own deployment; beside
// the stand-in model every one calls the stand-in with no credential. A kind's settings with no
// model of the kind listed are refused, and so is a listed kind with none of its settings.
func TestTheTestModels(t *testing.T) {
	models := resolveWith(t, testModelsEnv(nil)).Writing.TestModels
	want := map[string]modelGroup{
		"grok-4.7": {Kind: "azure", Endpoint: "https://models.example/models", Deployment: "grok-4.7", Scope: embed.DefaultEntraScope,
			TokensPerMinute: 60000, TenantID: "tenant-id", ClientID: "client-id", FederatedToken: "/var/run/secrets/azure-federation/token"},
		"claude-haiku-5-5": {Kind: "anthropic", Endpoint: gatewayEndpoint, Deployment: "claude-haiku-5-5",
			KeyFile: "/var/run/model-gateway-key/key", TokensPerMinute: 40000},
	}
	if !maps.Equal(models, want) {
		t.Errorf("TestModels = %+v, want %+v", models, want)
	}
	if models := resolveWith(t, nil).Writing.TestModels; models != nil {
		t.Errorf("unset: TestModels = %+v, want none", models)
	}
	refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_HOOKS": ""}), "DISPATCH_TEST_HOOKS=1 required with DISPATCH_TEST_MODELS")

	standIn := resolveWith(t, map[string]string{
		"DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_MODEL_URL": "http://127.0.0.1:9040",
		"DISPATCH_TEST_MODELS": "grok-4.7=azure", "DISPATCH_TEST_MODELS_AZURE_TOKENS_PER_MINUTE": "40000",
	}).Writing.TestModels
	if got, want := standIn["grok-4.7"], (modelGroup{Kind: "azure", Endpoint: "http://127.0.0.1:9040", Deployment: "grok-4.7", TokensPerMinute: 40000, StandIn: true}); got != want {
		t.Errorf("a test model beside the stand-in = %+v, want %+v", got, want)
	}
	refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODEL_URL": "http://127.0.0.1:9040"}),
		"set beside DISPATCH_TEST_MODEL_URL")

	refusedWith(t, testModelsEnv(withOverrides(noAzureSignIn, map[string]string{"DISPATCH_TEST_MODELS": "claude-haiku-5-5=anthropic"})),
		"DISPATCH_TEST_MODELS_AZURE_ENDPOINT, DISPATCH_TEST_MODELS_AZURE_TOKENS_PER_MINUTE set, but DISPATCH_TEST_MODELS names no azure model")
	refusedWith(t, map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_MODELS": "grok-4.7=azure"},
		"DISPATCH_TEST_MODELS names an azure model, but no DISPATCH_TEST_MODELS_AZURE_* setting is set")
}

// leastCharged is the least a call of a group is charged under the default rule sets, as the boot
// weighs it: the pacer's charge (modelpace.Charge) of the shortest system prompt of the calls the
// group sends, with its reply bound. The reviewer's group sends reviews, Rewrite and Define's
// rewrites and definitions, and a test model, which a request names in place of either group, all
// three; the records job sends its selection prompt alone.
func leastCharged(t *testing.T, prefix string) (least, bound int) {
	t.Helper()
	if prefix == wordRecordsModel {
		return modelpace.Charge(utf8.RuneCountInString(wordrecords.SelectPrompt), wordrecords.CallOutputTokens), wordrecords.CallOutputTokens
	}
	purposes := map[string][]string{
		reviewerModel:      {reviewer.PurposeReview},
		rewriteDefineModel: {reviewer.PurposeRewrite, reviewer.PurposeDefine},
		testModelsPrefix:   {reviewer.PurposeReview, reviewer.PurposeRewrite, reviewer.PurposeDefine},
	}[prefix]
	runes, err := reviewer.LeastSystemPromptRunes(reviewer.DefaultRules, purposes...)
	if err != nil {
		t.Fatal(err)
	}
	return modelpace.Charge(runes, reviewer.DefaultOutputTokens), reviewer.DefaultOutputTokens
}

// Each call is charged its prompt's estimated tokens and its whole reply bound before it is sent,
// and no call's prompt is shorter than the shortest system prompt of the calls the group sends, so
// a minute's quota not more than that charge could serve no call. A set group's ceiling at that
// charge refuses the boot, naming the setting, its value, the group's calls, the charge and the
// bound, and a ceiling one token more starts. Each group is held to its own calls' prompts: the
// reviewer's to its reviews, and Rewrite and Define's to their rewrites and definitions, whose
// shortest prompt is another length, so a floor taken from the other group's prompts starts a
// group whose calls the pacer refuses or refuses one whose calls it serves. The rule sets of
// DISPATCH_REVIEWER_RULES set the reviewer's shortest prompt, so a ceiling that fits the structure
// rule set alone is refused under the default ones. So it is with the quota
// DISPATCH_MODEL_DEPLOYMENT_QUOTAS names for the group's deployment, and a quota for a deployment
// no group names is not judged by this rule. A group beside the stand-in model is paced as any
// other, so it is held to the same charge. A group on the answerer's deployment (the same kind,
// endpoint host and deployment) is paced at the answerer's quota where no quota names that
// deployment, so that quota is held to it too; an answerer on another endpoint host, of another
// deployment or beside a group of another kind is another limiter and is not judged. The test
// models serve the reviewer's reviews and Rewrite and Define's rewrites and definitions, so each
// kind's ceiling and each test model's quota are held to the charge of the shortest of those.
func TestAMinutesQuotaNoCallFitsRefusesBoot(t *testing.T) {
	const (
		quotas        = "DISPATCH_MODEL_DEPLOYMENT_QUOTAS"
		answererQuota = "DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE"
		why           = ": each call is charged its prompt's estimated tokens and its whole reply bound before it is sent, so a minute's quota no larger could serve no call"
	)
	// starts fails the test where the boot refuses env.
	starts := func(t *testing.T, what string, env map[string]string) {
		t.Helper()
		if _, err := resolveBootConfig(bootEnvironment(env)); err != nil {
			t.Errorf("%s: the boot is refused: %v", what, err)
		}
	}
	reviews, _ := leastCharged(t, reviewerModel)
	if rewrites, _ := leastCharged(t, rewriteDefineModel); reviews == rewrites {
		t.Fatalf("the reviews and the rewrites and definitions are both charged %d at the least: no row below tells a floor taken from the other group's prompts", reviews)
	}
	for _, row := range []struct{ prefix, whose, calls string }{
		{reviewerModel, "the reviewer's", "reviews"},
		{rewriteDefineModel, "Rewrite and Define's", "rewrites and definitions"},
		{wordRecordsModel, "the records job's", "calls"},
	} {
		t.Run(row.prefix, func(t *testing.T) {
			least, bound := leastCharged(t, row.prefix)
			if least <= bound {
				t.Fatalf("the least charge %d is not more than the reply bound %d: the system prompt adds nothing", least, bound)
			}
			ceiling, at, over := row.prefix+"TOKENS_PER_MINUTE", strconv.Itoa(least), strconv.Itoa(least+1)
			charged := strconv.Itoa(least) + ", the least " + row.whose + " " + row.calls + " are charged (a reply bound of " + strconv.Itoa(bound) + " tokens and the shortest system prompt of those calls)"
			refusedWith(t, anthropicGroup(row.prefix, map[string]string{ceiling: at}), ceiling+"="+at+" is not more than "+charged+why)
			refusedWith(t, standInGroup(row.prefix, "azure", map[string]string{ceiling: at}), ceiling+"="+at+" is not more than "+charged+why)
			if got := groupOf[row.prefix](resolveWith(t, anthropicGroup(row.prefix, map[string]string{ceiling: over})).Writing).TokensPerMinute; got != least+1 {
				t.Errorf("a ceiling one over the charge: TokensPerMinute = %d, want %d", got, least+1)
			}
			if row.prefix == reviewerModel {
				runes, err := reviewer.LeastSystemPromptRunes([]reviewer.RuleSet{reviewer.RuleStructure}, reviewer.PurposeReview)
				if err != nil {
					t.Fatal(err)
				}
				fits := strconv.Itoa(modelpace.Charge(runes, bound) + 1)
				starts(t, "a ceiling over the charge of the structure rule set alone", anthropicGroup(row.prefix, map[string]string{ceiling: fits, "DISPATCH_REVIEWER_RULES": "structure"}))
				refusedWith(t, anthropicGroup(row.prefix, map[string]string{ceiling: fits}), ceiling+"="+fits+" is not more than "+charged+why)
			}

			// anthropicGroup's deployment is claude-haiku-4-5-20251001, and grok-4.7 is no group's.
			const deployment = "claude-haiku-4-5-20251001"
			refusedWith(t, anthropicGroup(row.prefix, map[string]string{quotas: deployment + "=" + at}),
				quotas+" names "+deployment+"="+at+", not more than "+charged+", which go to that deployment ("+row.prefix+"DEPLOYMENT)"+why)
			got := resolveWith(t, anthropicGroup(row.prefix, map[string]string{quotas: deployment + "=" + over + ",grok-4.7=1"})).Writing.DeploymentQuotas
			if want := map[string]int{deployment: least + 1, "grok-4.7": 1}; !maps.Equal(got, want) {
				t.Errorf("a quota one over the charge, and one for a deployment no group names: DeploymentQuotas = %v, want %v", got, want)
			}

			// The answerer's deployment is chat-deployment at chat.example (azureChatEnvironment).
			// An azure group that names it at that host shares the answerer's limiter, whatever
			// the path of its endpoint.
			onAnswerer := func(quota string, overrides map[string]string) map[string]string {
				return azureChatEnvironment(withOverrides(azureGroup(row.prefix, map[string]string{
					answererQuota: quota, row.prefix + "ENDPOINT": "https://chat.example/openai/v1", row.prefix + "DEPLOYMENT": "chat-deployment",
				}), overrides))
			}
			refusedWith(t, onAnswerer(at, nil), answererQuota+"="+at+" is not more than "+charged+
				", which go to the answerer's deployment ("+row.prefix+"DEPLOYMENT) and are paced at its quota, since "+quotas+" names none for it"+why)
			starts(t, "the answerer's quota one over the charge", onAnswerer(over, nil))
			starts(t, "a group on another endpoint host", onAnswerer(at, map[string]string{row.prefix + "ENDPOINT": "https://models.example/models"}))
			starts(t, "a group of another deployment", onAnswerer(at, map[string]string{row.prefix + "DEPLOYMENT": "grok-4.7"}))
			starts(t, "a quota that names the answerer's deployment", onAnswerer(at, map[string]string{quotas: "chat-deployment=" + over}))
			starts(t, "a group of another kind", azureChatEnvironment(anthropicGroup(row.prefix, map[string]string{
				answererQuota: at, row.prefix + "ENDPOINT": "https://chat.example", row.prefix + "DEPLOYMENT": "chat-deployment",
			})))
		})
	}
	t.Run("the test models", func(t *testing.T) {
		least, bound := leastCharged(t, testModelsPrefix)
		at, over := strconv.Itoa(least), strconv.Itoa(least+1)
		for kind, model := range map[string]string{"azure": "grok-4.7", "anthropic": "claude-haiku-5-5"} {
			ceiling := "DISPATCH_TEST_MODELS_" + strings.ToUpper(kind) + "_TOKENS_PER_MINUTE"
			charged := at + ", the least the " + kind + " test models' reviews, rewrites and definitions are charged (a reply bound of " + strconv.Itoa(bound) + " tokens and the shortest system prompt of those calls)"
			refusedWith(t, testModelsEnv(map[string]string{ceiling: at}), ceiling+"="+at+" is not more than "+charged+why)
			if got := resolveWith(t, testModelsEnv(map[string]string{ceiling: over})).Writing.TestModels[model].TokensPerMinute; got != least+1 {
				t.Errorf("%s one over the charge: TokensPerMinute = %d, want %d", ceiling, got, least+1)
			}
			refusedWith(t, testModelsEnv(map[string]string{quotas: model + "=" + at}),
				quotas+" names "+model+"="+at+", not more than "+charged+", which go to that deployment (DISPATCH_TEST_MODELS)"+why)
			if got := resolveWith(t, testModelsEnv(map[string]string{quotas: model + "=" + over})).Writing.DeploymentQuotas[model]; got != least+1 {
				t.Errorf("%s's quota one over the charge: %d, want %d", model, got, least+1)
			}
		}
		// An azure test model the answerer's deployment names, at the answerer's endpoint host.
		onAnswerer := func(quota string) map[string]string {
			return azureChatEnvironment(testModelsEnv(map[string]string{
				answererQuota: quota, "DISPATCH_TEST_MODELS": "chat-deployment=azure,claude-haiku-5-5=anthropic",
				"DISPATCH_TEST_MODELS_AZURE_ENDPOINT": "https://chat.example/models",
			}))
		}
		refusedWith(t, onAnswerer(at), answererQuota+"="+at+" is not more than "+at+", the least the azure test models' reviews, rewrites and definitions are charged (a reply bound of "+strconv.Itoa(bound)+
			" tokens and the shortest system prompt of those calls), which go to the answerer's deployment (DISPATCH_TEST_MODELS) and are paced at its quota, since "+quotas+" names none for it"+why)
		starts(t, "the answerer's quota one over the charge", onAnswerer(over))
	})
}

// The boot refuses the answerer's quota exactly where the pacer would refuse a group's call for
// good. Each row is the reviewer's group beside an answerer whose quota is the least the group's
// reviews are charged. The boot is asked first. Then the answerer and the group join a pacer as
// main joins them (newAnswerer, modelGroup.pace), and the group is charged a review of the
// shortest review prompt and a user message of one token, as it charges every call
// (modelpace.Caller.WaitFor): the pacer refuses it for good in the rows the boot refuses, and
// serves it in the others. The boot models how the pacer picks a deployment's rate and what it
// charges, and this holds the two together.
func TestTheBootRefusesTheAnswerersQuotaWhereThePacerRefusesTheCall(t *testing.T) {
	least, bound := leastCharged(t, reviewerModel)
	runes, err := reviewer.LeastSystemPromptRunes(reviewer.DefaultRules, reviewer.PurposeReview)
	if err != nil {
		t.Fatal(err)
	}
	at, over := strconv.Itoa(least), strconv.Itoa(least+1)
	for name, row := range map[string]struct {
		overrides map[string]string
		refused   bool
	}{
		"a group on the answerer's deployment":         {nil, true},
		"a group on another endpoint host":             {map[string]string{reviewerModel + "ENDPOINT": "https://models.example/models"}, false},
		"a group of another deployment":                {map[string]string{reviewerModel + "DEPLOYMENT": "grok-4.7"}, false},
		"a quota that names the answerer's deployment": {map[string]string{"DISPATCH_MODEL_DEPLOYMENT_QUOTAS": "chat-deployment=" + over}, false},
	} {
		t.Run(name, func(t *testing.T) {
			env := func(answererQuota string) map[string]string {
				return azureChatEnvironment(withOverrides(azureGroup(reviewerModel, map[string]string{
					"DISPATCH_AZURE_CHAT_TOKENS_PER_MINUTE": answererQuota,
					reviewerModel + "ENDPOINT":              "https://chat.example/openai/v1",
					reviewerModel + "DEPLOYMENT":            "chat-deployment",
				}), row.overrides))
			}
			if _, err := resolveBootConfig(bootEnvironment(env(at))); (err != nil) != row.refused {
				t.Errorf("the boot: %v, want refused %t", err, row.refused)
			}
			// The settings resolve with the answerer's quota over the charge, and it is lowered
			// to it after.
			boot := resolveWith(t, env(over))
			boot.Answerer.TokensPerMinute = least
			pacer := modelpace.New(modelpace.Config{Quotas: boot.Writing.DeploymentQuotas})
			if _, err := newAnswerer(boot.Answerer, pacer); err != nil {
				t.Fatalf("newAnswerer: %v", err)
			}
			pace, err := boot.Writing.ReviewerModel.pace(pacer)
			if err != nil {
				t.Fatal(err)
			}
			var want error
			if row.refused {
				want = modelpace.ErrPastDeadline
			}
			if err := pace.WaitFor(t.Context(), runes+modelpace.CharsPerToken, bound); !errors.Is(err, want) {
				t.Errorf("the pacer, charged the shortest call: %v, want %v", err, want)
			}
		})
	}
}

// An anthropic group's call carries its key, so its endpoint is http only to a host inside the
// machine or the cluster: localhost, a loopback address, or a cluster Service's name. http to any
// other host refuses the boot, naming the setting, for a group and for the anthropic test models,
// and https to it starts.
func TestAnAnthropicGroupCallsPlainHTTPOnlyInsideTheCluster(t *testing.T) {
	for _, endpoint := range []string{
		gatewayEndpoint, "http://model-gateway.model-gateway.svc:4000", "http://localhost:4000", "http://127.0.0.1:4000",
		"http://[::1]:4000", "https://model-gateway.example:4000",
	} {
		resolveWith(t, anthropicGroup(reviewerModel, map[string]string{reviewerModel + "ENDPOINT": endpoint}))
		resolveWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_ANTHROPIC_ENDPOINT": endpoint}))
	}
	for _, endpoint := range []string{"http://model-gateway.example:4000", "http://10.0.0.7:4000", "http://model-gateway:4000", "http://svc.example"} {
		refusedWith(t, anthropicGroup(reviewerModel, map[string]string{reviewerModel + "ENDPOINT": endpoint}),
			reviewerModel+"ENDPOINT="+strconv.Quote(endpoint)+" is http to a host outside the cluster, which would carry the reviewer's model group's key in clear text")
		refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_ANTHROPIC_ENDPOINT": endpoint}),
			"DISPATCH_TEST_MODELS_ANTHROPIC_ENDPOINT="+strconv.Quote(endpoint)+" is http to a host outside the cluster")
	}
}

// writingSettingCases are the cases TestEverySettingReachesItsReader runs for the rows of the work
// on agents' texts: each hands its row to resolveWritingSettings through the table.
func writingSettingCases() map[string]func(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"DISPATCH_REVIEWER_RULES": func(t *testing.T) {
			if got := resolveWith(t, nil).Writing.ReviewerRules; !slices.Equal(got, reviewer.DefaultRules) {
				t.Errorf("unset: ReviewerRules = %v", got)
			}
			if got := resolveWith(t, map[string]string{"DISPATCH_REVIEWER_RULES": " detector, structure,,detector "}).Writing.ReviewerRules; !slices.Equal(got, []reviewer.RuleSet{reviewer.RuleDetector, reviewer.RuleStructure}) {
				t.Errorf("ReviewerRules = %v, want detector and structure, each once", got)
			}
			refusedWith(t, map[string]string{"DISPATCH_REVIEWER_RULES": "noslop,passive-voice"}, `DISPATCH_REVIEWER_RULES names "passive-voice"`)
		},
		"DISPATCH_REVIEWER_TIMEOUT":       durationCase("DISPATCH_REVIEWER_TIMEOUT", reviewer.DefaultTimeout, func(w writingSettings) time.Duration { return w.ReviewerTimeout }),
		"DISPATCH_REVIEWER_WRITER_WINDOW": durationCase("DISPATCH_REVIEWER_WRITER_WINDOW", api.DefaultWriterWindow, func(w writingSettings) time.Duration { return w.ReviewerWriterWindow }),
		"DISPATCH_REVIEWER_UNEDITED": func(t *testing.T) {
			if got := resolveWith(t, nil).Writing.ReviewerUnedited; got != api.UneditedRewrite {
				t.Errorf("unset: ReviewerUnedited = %q, want rewrite", got)
			}
			if got := resolveWith(t, map[string]string{"DISPATCH_REVIEWER_UNEDITED": " as_written "}).Writing.ReviewerUnedited; got != api.UneditedAsWritten {
				t.Errorf("ReviewerUnedited = %q, want as_written", got)
			}
			refusedWith(t, map[string]string{"DISPATCH_REVIEWER_UNEDITED": "post"}, `DISPATCH_REVIEWER_UNEDITED="post" (expected rewrite or as_written)`)
		},
		"DISPATCH_WORD_RECORDS": func(t *testing.T) {
			if !resolveWith(t, nil).Writing.WordRecords {
				t.Error("unset: WordRecords off, want on")
			}
			if resolveWith(t, map[string]string{"DISPATCH_WORD_RECORDS": "off"}).Writing.WordRecords {
				t.Error("off: WordRecords on")
			}
			refusedWith(t, map[string]string{"DISPATCH_WORD_RECORDS": "1"}, `DISPATCH_WORD_RECORDS="1" (expected on or off)`)
		},
		"DISPATCH_WORD_RECORDS_FROM": func(t *testing.T) {
			if got := resolveWith(t, nil).Writing.WordRecordsFrom; !got.Equal(time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("unset: WordRecordsFrom = %s", got)
			}
			if got := resolveWith(t, map[string]string{"DISPATCH_WORD_RECORDS_FROM": " 2026-11-01T12:00:00+01:00 "}).Writing.WordRecordsFrom; !got.Equal(time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC)) {
				t.Errorf("WordRecordsFrom = %s", got)
			}
			refusedWith(t, map[string]string{"DISPATCH_WORD_RECORDS_FROM": "2026-11-01"}, `DISPATCH_WORD_RECORDS_FROM="2026-11-01" (expected an RFC 3339 time`)
		},
		"DISPATCH_WRITING_RULES_REFUSAL": func(t *testing.T) {
			if resolveWith(t, nil).Writing.WritingRulesRefusal {
				t.Error("unset: the refusal is on, want off")
			}
			if !resolveWith(t, map[string]string{"DISPATCH_WRITING_RULES_REFUSAL": " on "}).Writing.WritingRulesRefusal {
				t.Error("on: the refusal is off")
			}
			refusedWith(t, map[string]string{"DISPATCH_WRITING_RULES_REFUSAL": "yes"}, `DISPATCH_WRITING_RULES_REFUSAL="yes" (expected on or off)`)
		},
		"DISPATCH_WRITING_RULES_EXTRA": func(t *testing.T) {
			if got := resolveWith(t, map[string]string{"DISPATCH_WRITING_RULES_EXTRA": "ask_parts, reader_words"}).Writing.WritingRulesExtra; !slices.Equal(got, []reviewer.RuleSet{reviewer.RuleAskParts, reviewer.RuleReaderWords}) {
				t.Errorf("WritingRulesExtra = %v", got)
			}
			refusedWith(t, map[string]string{"DISPATCH_WRITING_RULES_EXTRA": "noslop"}, `DISPATCH_WRITING_RULES_EXTRA names "noslop", which is not one of reader_words, ask_parts`)
		},
		"DISPATCH_MODEL_DEPLOYMENT_QUOTAS": func(t *testing.T) {
			got := resolveWith(t, map[string]string{"DISPATCH_MODEL_DEPLOYMENT_QUOTAS": " grok-4.7=600000 , claude-haiku-4-5-20251001 = 400000,"}).Writing.DeploymentQuotas
			if want := map[string]int{"grok-4.7": 600000, "claude-haiku-4-5-20251001": 400000}; !maps.Equal(got, want) {
				t.Errorf("DeploymentQuotas = %v, want %v", got, want)
			}
			for _, bad := range []string{"grok-4.7", "grok-4.7=", "=600000", "grok-4.7=0", "grok-4.7=+5", "grok-4.7=six"} {
				refusedWith(t, map[string]string{"DISPATCH_MODEL_DEPLOYMENT_QUOTAS": bad}, "DISPATCH_MODEL_DEPLOYMENT_QUOTAS names "+`"`+bad+`"`)
			}
			refusedWith(t, map[string]string{"DISPATCH_MODEL_DEPLOYMENT_QUOTAS": "grok-4.7=1,grok-4.7=2"}, "DISPATCH_MODEL_DEPLOYMENT_QUOTAS names grok-4.7 twice")
		},
		"DISPATCH_TEST_MODEL_URL": func(t *testing.T) {
			if got := resolveWith(t, map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_MODEL_URL": " http://127.0.0.1:9040 "}).Writing.TestModelURL; got != "http://127.0.0.1:9040" {
				t.Errorf("TestModelURL = %q", got)
			}
			refusedWith(t, map[string]string{"DISPATCH_TEST_MODEL_URL": "http://127.0.0.1:9040"}, "DISPATCH_TEST_HOOKS=1 required with DISPATCH_TEST_MODEL_URL")
			for _, bad := range []string{"127.0.0.1:9040", "ftp://127.0.0.1:9040", "http://127.0.0.1:9040/?x=1", "http:///v1"} {
				refusedWith(t, map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_MODEL_URL": bad}, "DISPATCH_TEST_MODEL_URL="+`"`+bad+`"`)
			}
		},
		"DISPATCH_TEST_MODELS": func(t *testing.T) {
			if got := slices.Sorted(maps.Keys(resolveWith(t, testModelsEnv(nil)).Writing.TestModels)); !slices.Equal(got, []string{"claude-haiku-5-5", "grok-4.7"}) {
				t.Errorf("TestModels = %v", got)
			}
			refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_HOOKS": ""}), "DISPATCH_TEST_HOOKS=1 required with DISPATCH_TEST_MODELS")
			for _, bad := range []string{"grok-4.7", "grok-4.7=openai", "=azure"} {
				refusedWith(t, testModelsEnv(withOverrides(noAzureSignIn, map[string]string{"DISPATCH_TEST_MODELS": bad})), "DISPATCH_TEST_MODELS names "+`"`+bad+`"`)
			}
			refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS": "grok-4.7=azure,grok-4.7=azure"}), "DISPATCH_TEST_MODELS names grok-4.7 twice")
		},
		"DISPATCH_TEST_MODELS_AZURE_ENDPOINT": func(t *testing.T) {
			if got := resolveWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_AZURE_ENDPOINT": " https://other.example/models "})).Writing.TestModels["grok-4.7"].Endpoint; got != "https://other.example/models" {
				t.Errorf("Endpoint = %q", got)
			}
			refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_AZURE_ENDPOINT": ""}), "DISPATCH_TEST_MODELS_AZURE_ENDPOINT missing")
			refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_AZURE_ENDPOINT": "http://models.example/models"}), `DISPATCH_TEST_MODELS_AZURE_ENDPOINT="http://models.example/models" (expected an absolute https URL`)
		},
		"DISPATCH_TEST_MODELS_AZURE_API_VERSION": func(t *testing.T) {
			if got := resolveWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_AZURE_API_VERSION": " 2024-05-01-preview "})).Writing.TestModels["grok-4.7"].APIVersion; got != "2024-05-01-preview" {
				t.Errorf("APIVersion = %q", got)
			}
		},
		"DISPATCH_TEST_MODELS_AZURE_SCOPE": func(t *testing.T) {
			if got := resolveWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_AZURE_SCOPE": " https://ai.azure.com/.default "})).Writing.TestModels["grok-4.7"].Scope; got != "https://ai.azure.com/.default" {
				t.Errorf("Scope = %q", got)
			}
			refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_AZURE_TENANT_ID": "", "DISPATCH_AZURE_CLIENT_ID": "", "DISPATCH_AZURE_FEDERATED_TOKEN_FILE": ""}), "needs the keyless Azure sign-in")
		},
		"DISPATCH_TEST_MODELS_AZURE_TOKENS_PER_MINUTE": func(t *testing.T) {
			if got := resolveWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_AZURE_TOKENS_PER_MINUTE": " 90000 "})).Writing.TestModels["grok-4.7"].TokensPerMinute; got != 90000 {
				t.Errorf("TokensPerMinute = %d, want 90000", got)
			}
			refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_AZURE_TOKENS_PER_MINUTE": ""}), "DISPATCH_TEST_MODELS_AZURE_TOKENS_PER_MINUTE missing")
		},
		"DISPATCH_TEST_MODELS_ANTHROPIC_ENDPOINT": func(t *testing.T) {
			if got := resolveWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_ANTHROPIC_ENDPOINT": " https://gateway.example "})).Writing.TestModels["claude-haiku-5-5"].Endpoint; got != "https://gateway.example" {
				t.Errorf("Endpoint = %q", got)
			}
			refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_ANTHROPIC_ENDPOINT": "http://gateway.example?x=1"}), `DISPATCH_TEST_MODELS_ANTHROPIC_ENDPOINT="http://gateway.example?x=1" (expected an absolute http or https URL`)
		},
		"DISPATCH_TEST_MODELS_ANTHROPIC_KEY_FILE": func(t *testing.T) {
			if got := resolveWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_ANTHROPIC_KEY_FILE": " /keys/gateway "})).Writing.TestModels["claude-haiku-5-5"].KeyFile; got != "/keys/gateway" {
				t.Errorf("KeyFile = %q", got)
			}
			refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_ANTHROPIC_KEY_FILE": ""}), "DISPATCH_TEST_MODELS_ANTHROPIC_KEY_FILE missing")
		},
		"DISPATCH_TEST_MODELS_ANTHROPIC_TOKENS_PER_MINUTE": func(t *testing.T) {
			if got := resolveWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_ANTHROPIC_TOKENS_PER_MINUTE": " 90000 "})).Writing.TestModels["claude-haiku-5-5"].TokensPerMinute; got != 90000 {
				t.Errorf("TokensPerMinute = %d, want 90000", got)
			}
			refusedWith(t, testModelsEnv(map[string]string{"DISPATCH_TEST_MODELS_ANTHROPIC_TOKENS_PER_MINUTE": "0"}), `DISPATCH_TEST_MODELS_ANTHROPIC_TOKENS_PER_MINUTE="0" (expected a positive integer)`)
		},
	}
	holdable := map[string]func(writingSettings) string{
		"DISPATCH_REVIEWER_ASKS":            func(w writingSettings) string { return w.ReviewAsks },
		"DISPATCH_REVIEWER_ISSUES":          func(w writingSettings) string { return w.ReviewIssues },
		"DISPATCH_REVIEWER_COMMENTS":        func(w writingSettings) string { return w.ReviewComments },
		"DISPATCH_REVIEWER_MESSAGES":        func(w writingSettings) string { return w.ReviewMessages },
		"DISPATCH_REVIEWER_DECISION_BLOCKS": func(w writingSettings) string { return w.ReviewDecisionBlocks },
	}
	for name, choiceOf := range holdable {
		cases[name] = reviewChoiceCase(name, []string{api.ReviewAfter, api.ReviewHold}, "off, after or hold", choiceOf)
	}
	cases["DISPATCH_REVIEWER_DOCUMENTS"] = func(t *testing.T) {
		reviewChoiceCase("DISPATCH_REVIEWER_DOCUMENTS", []string{api.ReviewAfter}, "off or after", func(w writingSettings) string { return w.ReviewDocuments })(t)
		refusedWith(t, anthropicGroup(reviewerModel, map[string]string{"DISPATCH_REVIEWER_DOCUMENTS": "hold"}), `DISPATCH_REVIEWER_DOCUMENTS="hold" (expected off or after)`)
	}
	for prefix, group := range groupOf {
		maps.Copy(cases, modelGroupCases(prefix, group))
	}
	return cases
}

// reviewChoiceCase is the case of a DISPATCH_REVIEWER_<KIND> row: off unset, each of choices
// with the reviewer's model group set, refused with no group naming that group, and any other
// value refused naming the values it takes.
func reviewChoiceCase(name string, choices []string, expected string, choiceOf func(writingSettings) string) func(t *testing.T) {
	return func(t *testing.T) {
		if got := choiceOf(resolveWith(t, nil).Writing); got != api.ReviewOff {
			t.Errorf("unset: %s = %q, want off", name, got)
		}
		for _, choice := range choices {
			if got := choiceOf(resolveWith(t, anthropicGroup(reviewerModel, map[string]string{name: " " + choice + " "})).Writing); got != choice {
				t.Errorf("%s = %q, want %s", name, got, choice)
			}
			refusedWith(t, map[string]string{name: choice}, name+"="+choice+" needs the reviewer's model group (DISPATCH_REVIEWER_MODEL_KIND")
		}
		refusedWith(t, anthropicGroup(reviewerModel, map[string]string{name: "on"}), name+`="on" (expected `+expected+")")
	}
}

// durationCase is the case of a duration row: fallback unset, the value set, and a value that is
// not a positive Go duration refused.
func durationCase(name string, fallback time.Duration, valueOf func(writingSettings) time.Duration) func(t *testing.T) {
	return func(t *testing.T) {
		if got := valueOf(resolveWith(t, nil).Writing); got != fallback {
			t.Errorf("unset: %s = %s, want %s", name, got, fallback)
		}
		if got := valueOf(resolveWith(t, map[string]string{name: " 45s "}).Writing); got != 45*time.Second {
			t.Errorf("%s = %s, want 45s", name, got)
		}
		for _, bad := range []string{"0s", "-1m", "45"} {
			refusedWith(t, map[string]string{name: bad}, name+`="`+bad+`" (expected a positive Go duration)`)
		}
	}
}

// modelGroupCases are the cases of one model group's seven rows.
func modelGroupCases(prefix string, groupOf func(writingSettings) modelGroup) map[string]func(t *testing.T) {
	group := func(t *testing.T, env map[string]string) modelGroup {
		t.Helper()
		return groupOf(resolveWith(t, env).Writing)
	}
	return map[string]func(t *testing.T){
		prefix + "KIND": func(t *testing.T) {
			if got := group(t, azureGroup(prefix, map[string]string{prefix + "KIND": " azure "})).Kind; got != "azure" {
				t.Errorf("Kind = %q, want azure", got)
			}
			if got := group(t, anthropicGroup(prefix, nil)).Kind; got != "anthropic" {
				t.Errorf("Kind = %q, want anthropic", got)
			}
			if got := group(t, nil); got != (modelGroup{}) {
				t.Errorf("unset: %+v, want no model", got)
			}
			refusedWith(t, azureGroup(prefix, map[string]string{prefix + "KIND": "openai"}), prefix+`KIND="openai" (expected azure or anthropic)`)
			// An azure group alone reads the shared sign-in; an anthropic group does not.
			refusedWith(t, anthropicGroup(prefix, azureFederated), "DISPATCH_AZURE_TENANT_ID, DISPATCH_AZURE_CLIENT_ID, DISPATCH_AZURE_FEDERATED_TOKEN_FILE set, but neither DISPATCH_EMBEDDER nor DISPATCH_ANSWERER is azure and no model group's kind is azure")
			refusedWith(t, azureGroup(prefix, map[string]string{"DISPATCH_AZURE_CLIENT_ID": ""}), "DISPATCH_AZURE_CLIENT_ID missing")
			refusedWith(t, azureGroup(prefix, map[string]string{"DISPATCH_AZURE_TENANT_ID": "", "DISPATCH_AZURE_CLIENT_ID": "", "DISPATCH_AZURE_FEDERATED_TOKEN_FILE": ""}), prefix+"KIND=azure needs the keyless Azure sign-in")
		},
		prefix + "ENDPOINT": func(t *testing.T) {
			if got := group(t, azureGroup(prefix, map[string]string{prefix + "ENDPOINT": " https://other.example/openai/v1 "})).Endpoint; got != "https://other.example/openai/v1" {
				t.Errorf("Endpoint = %q", got)
			}
			if got := group(t, anthropicGroup(prefix, map[string]string{prefix + "ENDPOINT": "https://gateway.example"})).Endpoint; got != "https://gateway.example" {
				t.Errorf("Endpoint = %q", got)
			}
			// Plain HTTP is an anthropic gateway's alone.
			refusedWith(t, azureGroup(prefix, map[string]string{prefix + "ENDPOINT": "http://models.example/models"}), prefix+`ENDPOINT="http://models.example/models" (expected an absolute https URL`)
			refusedWith(t, anthropicGroup(prefix, map[string]string{prefix + "ENDPOINT": "http://gateway.example?x=1"}), prefix+`ENDPOINT="http://gateway.example?x=1" (expected an absolute http or https URL`)
		},
		prefix + "DEPLOYMENT": func(t *testing.T) {
			if got := group(t, azureGroup(prefix, map[string]string{prefix + "DEPLOYMENT": " Kimi-K3.1-Pro "})).Deployment; got != "Kimi-K3.1-Pro" {
				t.Errorf("Deployment = %q", got)
			}
		},
		prefix + "API_VERSION": func(t *testing.T) {
			if got := group(t, azureGroup(prefix, map[string]string{prefix + "API_VERSION": " 2024-05-01-preview "})).APIVersion; got != "2024-05-01-preview" {
				t.Errorf("APIVersion = %q", got)
			}
			refusedWith(t, anthropicGroup(prefix, map[string]string{prefix + "API_VERSION": "2024-05-01-preview"}), prefix+"API_VERSION set, but "+prefix+"KIND is anthropic")
		},
		prefix + "SCOPE": func(t *testing.T) {
			if got := group(t, azureGroup(prefix, map[string]string{prefix + "SCOPE": " https://ai.azure.com/.default "})).Scope; got != "https://ai.azure.com/.default" {
				t.Errorf("Scope = %q", got)
			}
			if got := group(t, azureGroup(prefix, nil)).Scope; got != embed.DefaultEntraScope {
				t.Errorf("unset: Scope = %q, want the embedder's", got)
			}
			refusedWith(t, anthropicGroup(prefix, map[string]string{prefix + "SCOPE": "https://ai.azure.com/.default"}), prefix+"SCOPE set, but "+prefix+"KIND is anthropic")
		},
		prefix + "KEY_FILE": func(t *testing.T) {
			if got := group(t, anthropicGroup(prefix, map[string]string{prefix + "KEY_FILE": " /keys/gateway "})).KeyFile; got != "/keys/gateway" {
				t.Errorf("KeyFile = %q", got)
			}
			refusedWith(t, azureGroup(prefix, map[string]string{prefix + "KEY_FILE": "/keys/gateway"}), prefix+"KEY_FILE set, but "+prefix+"KIND is azure")
		},
		prefix + "TOKENS_PER_MINUTE": func(t *testing.T) {
			if got := group(t, anthropicGroup(prefix, map[string]string{prefix + "TOKENS_PER_MINUTE": " 90000 "})).TokensPerMinute; got != 90000 {
				t.Errorf("TokensPerMinute = %d, want 90000", got)
			}
			refusedWith(t, anthropicGroup(prefix, map[string]string{prefix + "TOKENS_PER_MINUTE": "0"}), prefix+`TOKENS_PER_MINUTE="0" (expected a positive integer)`)
		},
	}
}
