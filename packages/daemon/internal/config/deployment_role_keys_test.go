package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// checkerRole is a role of the deployment, `checker`, with its prompt file beside the
// configuration (rolesFile writes it).
const checkerRole = "roles: {checker: {type: testing, prompt_file: checker.md, model: checker-model}}\n"

// worker_backends takes a role the deployment defines as a key, and Claude Code runs it; a file
// that defines no such role is refused with the text of any other name that is no issue role.
func TestWorkerBackendsTakesARoleOfTheDeployment(t *testing.T) {
	cfg, err := Load(rolesFile(t, checkerRole+"worker_backends: {checker: claude}\n", "checker.md"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.BackendFor("checker"); got != runtime.BackendClaude {
		t.Errorf("BackendFor(checker) = %q, want claude", got)
	}

	_, err = Load(rolesFile(t, "worker_backends: {checker: claude}\n"), noEnv)
	want := `worker_backends names "checker", which is not an issue role (architect, planner, implementer, tester, reviewer, merger)`
	if err == nil || err.Error() != want {
		t.Fatalf("Load without roles = %v, want %q", err, want)
	}
}

// The check across keys runs once every key it judges is set, so a name no role holds is refused
// through the daemon's load and through the check's load alike, with a list that names the roles
// of the deployment beside Legion's; a name outside the role grammar is refused when the key is
// read, before any other key is, and one with no key's shape, such as one holding an upper-case
// letter, is named by its line alone (unknownKeyAt).
func TestTheCheckAcrossKeysSeesEveryKeyItJudges(t *testing.T) {
	want := `worker_backends names "ghost", which is not an issue role (architect, planner, implementer, tester, reviewer, merger, checker)`
	path := rolesFile(t, checkerRole+"worker_backends: {ghost: omp}\n", "checker.md")
	for name, load := range map[string]func(string, func(string) string) (Config, error){"Load": Load, "LoadForValidation": LoadForValidation} {
		if _, err := load(path, noEnv); err == nil || err.Error() != want {
			t.Errorf("%s = %v, want %q", name, err, want)
		}
	}

	_, err := Load(rolesFile(t, "worker_backends: {Checker: omp}\n"), noEnv)
	want = "unknown key at line 18 under worker_backends, whose name has no key's shape (" + keyShapeWords +
		"): it must be an issue role (architect, planner, implementer, tester, reviewer, merger)"
	if err == nil || err.Error() != want {
		t.Fatalf("Load of a name outside the grammar = %v, want %q", err, want)
	}
}

// fakeToken is a made-up token of a real shape, none of them a credential: shape names it, and
// mixed is whether it holds an upper-case letter, a "_", a "/" or a "+", which no lower-case
// grammar (the role grammar, a DNS name's, a bucket's) takes.
type fakeToken struct {
	shape, text string
	mixed       bool
}

// fakeTokens are the tokens the tests of a refusal's text paste where a word, a name or a path
// goes: a 32-hex API key that starts with a letter, a short lower-case base36 token and a 40-hex
// token, each of lower case, and a `ghp_` token of 36 base62 characters, a 40-character base64
// token and an `sk-ant-api03-` token, each of mixed case. Each prefix is joined at run time, as
// cmd/legion's ghFakeToken is, so the source holds no literal of a credential's shape for a leak
// check to stop.
func fakeTokens() []fakeToken {
	return []fakeToken{
		{shape: "32-hex", text: "deadbeefcafe0123456789abcdef0123"},
		{shape: "base36", text: "kq7zm2xw9rt4pvn8ys3lgj5"},
		{shape: "40-hex", text: "deadbeefcafe0123456789abcdef0123456789ab"},
		{shape: "ghp", text: "ghp" + "_" + "FAKE0tokenONLY1forTHE2configTESTS3xZ", mixed: true},
		{shape: "base64", text: "FAKEsecretKEY/forTHE+configTESTS/onlyAB1", mixed: true},
		{shape: "sk-ant", text: "sk-ant" + "-api03-" + strings.Repeat("Fake0Key_For0Config-Tests0Only-", 3) + "AA", mixed: true},
	}
}

// holdsNoPiece fails t when err's text holds any 8-character piece of token.
func holdsNoPiece(t *testing.T, err error, token string) {
	t.Helper()
	for i := 0; i+8 <= len(token); i++ {
		if strings.Contains(err.Error(), token[i:i+8]) {
			t.Fatalf("the refusal %q holds %q of the token", err, token[i:i+8])
		}
	}
}

// A token pasted where a role's name or a type goes is named by its line, its place or its field,
// never as written. Two shapes fit the role grammar (claim.IsRole, `^[a-z][a-z0-9-]{0,31}$`): a
// 32-hex API key that starts with a letter, and a short lower-case base36 token; each reader
// refuses them for holding a token's run (tokenShaped). The others fall outside the grammar and
// hold no such run, so the grammar check is the only guard against them: a `ghp_` token of 36
// base62 characters, a 40-character base64 token and an `sk-ant-api03-` token, each of mixed case,
// and a 40-hex token, which is longer than the grammar's 32 characters. Every reader of a role's
// name or a type is tried: the three role-keyed settings, a role `roles` defines, a role's type,
// `role_types`, a hook's `roles`, a project's `default_steps`, an experiment's `roles`, and a
// project's `production_check`, which takes the tester's name or `off` and names another word only
// when it has a key's shape (keyShape). A `default_steps` entry is judged before it is parsed as a
// step, so an entry no step's grammar takes (a 32-hex key that starts with a digit, or a role and a
// token in one entry) is named by its number too. The control is a role of the grammar that no
// token is (`ghost`), named as written by the check across keys.
//
// It reads the refusal's text for each 8-character piece of the token, not whether a guard ran or
// which error was returned. The leak this test exists for was a guard that ran, on a path the
// printer had already passed: a check that a guard was called would have passed then too. Keep it
// on the text whatever the readers become.
func TestATokenThatFitsTheRoleGrammarIsNeverPrinted(t *testing.T) {
	for _, token := range fakeTokens() {
		if token.mixed && tokenShaped(token.text) {
			t.Fatalf("the token %.7s… holds a token's run, so it does not show that the grammar refuses a token tokenShaped misses", token.text)
		}
	}
	hookOf := func(role string) string {
		return "hooks: [{name: nudge, on: work_back, every: 1, roles: [" + role + "], prompt: Look again.}]\n"
	}
	stepsOf := func(role string) string {
		return strings.Replace(minimalFile, "projects:\n  DEMO: {}\n", "projects:\n  DEMO: {default_steps: [implementer, "+role+"]}\n", 1)
	}
	for _, token := range fakeTokens() {
		// The first line after minimalFile is line 18; the line of the DEMO project is line 7.
		for name, reader := range map[string]struct{ body, place string }{
			"worker_backends":             {minimalFile + "worker_backends: {" + token.text + ": omp}\n", " at line 18 "},
			"role_config_files":           {minimalFile + "role_config_files: {" + token.text + ": [/etc/a.yml]}\n", " at line 18 "},
			"worker_stall_backstop_hours": {minimalFile + "worker_stall_backstop_hours: {" + token.text + ": 6}\n", " at line 18 "},
			"roles":                       {minimalFile + "roles: {" + token.text + ": {type: testing, prompt_file: checker.md}}\n", " at line 18 "},
			"a role's type":               {minimalFile + "roles: {checker: {type: " + token.text + ", prompt_file: checker.md}}\n", "roles.checker.type "},
			"role_types":                  {minimalFile + "role_types: [security, " + token.text + "]\n", "role_types entry 2 "},
			"a hook's roles":              {minimalFile + hookOf(token.text), "hooks[0].roles entry 1 "},
			"default_steps":               {stepsOf(token.text), "projects.DEMO.default_steps entry 2 "},
			"an experiment's roles": {experimentsFile("{alpha: {roles: [implementer, "+token.text+"], variants: [{name: only}]}}", ""),
				"projects.DEMO.experiments.alpha.roles entry 2 "},
			"production_check": {productionCheckFile(token.text), "projects.DEMO.production_check at line 7 "},
		} {
			t.Run(name+"/"+token.shape, func(t *testing.T) {
				_, err := Load(configFileWith(t, reader.body, "checker.md"), noEnv)
				if err == nil {
					t.Fatalf("Load took the token as a role's name or a type, want it refused")
				}
				if !strings.Contains(err.Error(), reader.place) {
					t.Fatalf("the refusal %q does not name %q", err, reader.place)
				}
				holdsNoPiece(t, err, token.text)
			})
		}
	}
	const digitToken = "0123456789abcdef0123456789abcdef"
	for name, entry := range map[string]string{
		"a 32-hex token that starts with a digit": digitToken,
		"a role and a token in one entry":         "implementer " + digitToken,
	} {
		t.Run("default_steps/"+name, func(t *testing.T) {
			_, err := Load(configFileWith(t, stepsOf(entry)), noEnv)
			if err == nil {
				t.Fatalf("Load took the entry %q, want it refused", entry)
			}
			if !strings.Contains(err.Error(), "default_steps entry 2 ") {
				t.Fatalf("the refusal %q names no entry 2", err)
			}
			holdsNoPiece(t, err, digitToken)
		})
	}
	want := `worker_backends names "ghost", which is not an issue role (architect, planner, implementer, tester, reviewer, merger)`
	if _, err := Load(rolesFile(t, "worker_backends: {ghost: omp}\n"), noEnv); err == nil || err.Error() != want {
		t.Fatalf("Load of a role of the grammar = %v, want it named as written: %q", err, want)
	}
}

// A token pasted where any other word, name or path goes is named by its line, its place or its
// field, never as written, as TestATokenThatFitsTheRoleGrammarIsNeverPrinted holds for a role's
// name: a word of a few a key takes (a hook's `on`, a backend, an experiment's `mode`, `unit` and a
// variant's `plan`, a role's `github_app`, `controller`, `runtime.tmux.take_over`, dispatch_url's
// scheme, a watched workflow's event), named as written only where it has a key's shape
// (shapelessWord); a word the reader holds to a pattern a token misses or fits (`project`, a Slack
// channel, a canary issue, a repository, the leak check's digest, a variant's name, a bucket, a
// ServiceAccount, a claim, a metadata member), named only where it has a key's shape; a
// repository, written where the reader takes one or as a name it already took (a list's entry
// twice, a rename's pair, two renames that cannot stand together), and an owner of a rename, each
// named only where it holds no token's run in any case; a watched workflow, named only where it
// has an entry's shape, whichever of its refusals the token meets, the token beside a repository
// or a file the reader refuses or beside an event it refuses for another fault; an
// exchange_audience that is the audience itself, named only where it has an audience's shape, a
// Workload Identity pool's among them only where the project before `.svc.id.goog` is a project
// id's, and a session database writer audience's only where its pool id and provider id hold no
// token's run, as a writer audience that a broker's token carries too is; a word that is no move of
// a hook's `moves`, a name two hooks or two volumes carry, a mount's volume and a label named
// twice, each by its place; and a path that fits no grammar of its key (an overlay, an
// `instructions`, an NFS export, a mount path, a role's `prompt_file`), never named. A key whose
// grammar takes a lower-case token as a name (a variant's name, a bucket, a ServiceAccount, a
// claim) is tried with the tokens of mixed case alone, which that grammar refuses; a row whose
// token stands where the reader takes a repository's name, an owner, a watched workflow's file or
// a writer audience's id is tried with the tokens that place takes, of mixed case among them where
// it takes capitals, since any other is refused as no such value first, as the rows of
// parseRepository and of a watched workflow's form show.
//
// It reads the refusal's text for each 8-character piece of the token, as its sibling does.
func TestATokenWhereAWordANameOrAPathGoesIsNeverPrinted(t *testing.T) {
	project := func(entry string) string {
		return strings.Replace(minimalFile, "  DEMO: {}\n", "  DEMO: {"+entry+"}\n", 1)
	}
	hooks := func(hooks string) string { return minimalFile + "hooks: [" + hooks + "]\n" }
	pod := func(lines string) string { return kubernetesFile + "    pod:\n" + lines }
	// asName, asOwner and asFile are the tokens GitHub serves as a repository's name and as an
	// owner, and a watched workflow takes as its file; asWriterID the tokens a writer audience
	// takes as its pool id and its provider id (ompsessions.ValidWriterAudience).
	asName := ghrepo.IsName
	asOwner := func(token string) bool { return repositoryShape.MatchString(token + "/a") }
	asFile := func(token string) bool { return workflowFilePattern.MatchString(token + ".yaml") }
	writerAudience := func(pool, provider string) string {
		return "//iam.googleapis.com/projects/123456789012/locations/global/workloadIdentityPools/" + pool + "/providers/" + provider
	}
	asWriterID := func(token string) bool { return ompsessions.ValidWriterAudience(writerAudience(token, token)) }
	// writerAudienceBeside is a session database whose writer_audience is writer and a broker whose
	// audience is brokerAudience.
	writerAudienceBeside := func(writer, brokerAudience string) string {
		return withSessionStore("    session_store: cloudsql-iam\n" +
			"    session_database: {instance: example-project:us-central1:example-instance, name: legion_sessions, user: legion-sessions@example-project.iam," +
			" impersonate_service_account: legion-sessions@example-project.iam.gserviceaccount.com, writer_audience: \"" + writer + "\"}\n" +
			"    agent_secrets:\n      url: https://secrets.internal.example\n" + agentSecretsTokenFile +
			"      audience: \"" + brokerAudience + "\"\n")
	}
	for _, token := range fakeTokens() {
		// The first line after minimalFile is line 18; the line of the DEMO project is line 7.
		for name, reader := range map[string]struct {
			body, place string
			// kubernetes rows load as `legion config check` does: kubernetesFile's App keys are
			// commands that fail.
			kubernetes bool
			// mixedOnly rows are tried with the tokens of mixed case alone: the key's grammar
			// takes a lower-case token.
			mixedOnly bool
			// takes, where set, holds the tokens the row is tried with: the row's token stands
			// where the reader takes a repository's name, an owner or a file (asName, asOwner,
			// asFile).
			takes func(token string) bool
		}{
			"a hook's moves": {body: hooks("{name: nudge, on: work_back, every: 1, roles: [implementer], moves: [" + token.text + "], prompt: Look again.}"),
				place: "hooks[0].moves entry 1 "},
			"a role's github_app": {body: minimalFile + "roles: {checker: {type: testing, prompt_file: checker.md, github_app: " + token.text + "}}\n",
				place: "roles.checker.github_app at line 18 "},
			"a hook's name twice": {body: hooks("{name: " + token.text + ", on: work_back, every: 1, roles: [implementer], prompt: A.}, {name: " + token.text + ", on: work_back, every: 1, roles: [tester], prompt: B.}"),
				place: "hooks[1].name is the name hooks[0] already carries"},
			"a hook's on": {body: hooks("{name: nudge, on: " + token.text + ", every: 1, roles: [implementer], prompt: A.}"),
				place: "hooks[0].on at line 18 "},
			"worker_backends' backend": {body: minimalFile + "worker_backends: {implementer: " + token.text + "}\n",
				place: "worker_backends.implementer at line 18 "},
			"an experiment's mode": {body: experimentsFile("{alpha: {mode: "+token.text+", variants: [{name: only}, {name: other}]}}", ""),
				place: "projects.DEMO.experiments.alpha.mode at line 7 "},
			"an experiment's unit": {body: experimentsFile("{alpha: {unit: "+token.text+", variants: [{name: only}]}}", ""),
				place: "projects.DEMO.experiments.alpha.unit at line 7 "},
			"an experiment's on_variant": {body: experimentsFile("{alpha: {mode: on, on_variant: "+token.text+", variants: [{name: only}, {name: other}]}}", ""),
				place: "projects.DEMO.experiments.alpha.on_variant at line 7 "},
			"a variant's plan": {body: experimentsFile("{alpha: {mode: random, unit: issue, variants: [{name: a, plan: "+token.text+"}, {name: b, plan: default}]}}", ""),
				place: "projects.DEMO.experiments.alpha.variants.a.plan at line 7 "},
			"a variant's name": {body: experimentsFile("{alpha: {variants: [{name: "+token.text+"}]}}", ""),
				place: "projects.DEMO.experiments.alpha.variants[0] name must match ", mixedOnly: true},
			"a variant's instructions": {body: experimentsFile("{alpha: {variants: [{name: only, instructions: "+token.text+"}]}}", ""),
				place: "projects.DEMO.experiments.alpha.variants.only.instructions must be "},
			"a variant's config_files": {body: experimentsFile("{alpha: {variants: [{name: only, config_files: ["+token.text+"]}]}}", ""),
				place: "projects.DEMO.experiments.alpha.variants.only.config_files entry 1 "},
			"role_config_files' overlay": {body: minimalFile + "role_config_files: {implementer: [" + token.text + "]}\n",
				place: "role_config_files.implementer entry 1 "},
			"a role's prompt_file": {body: minimalFile + "roles: {checker: {type: testing, prompt_file: " + token.text + "}}\n",
				place: "roles.checker.prompt_file names a file that could not be read"},
			"controller": {body: minimalFile + "controller: " + token.text + "\n",
				place: "controller has no key's shape"},
			"project": {body: strings.Replace(minimalFile, "project: DEMO\n", "project: "+token.text+"\n", 1),
				place: "project is no Dispatch project key"},
			"a project's slack_channels": {body: project("slack_channels: [" + token.text + "]"),
				place: "projects.DEMO.slack_channels entry 1 "},
			"a project's commit_issue_trailer_repos": {body: project("commit_issue_trailer_repos: [" + token.text + "]"),
				place: "projects.DEMO.commit_issue_trailer_repos[0] must be "},
			"a project's commit_issue_trailer_repos, ending in .git": {body: project("commit_issue_trailer_repos: [acme/" + token.text + ".git]"),
				place: "projects.DEMO.commit_issue_trailer_repos[0] must be "},
			"one commit_issue_trailer_repos repository twice": {body: project("commit_issue_trailer_repos: [acme/" + token.text + ", ACME/" + token.text + "]"),
				place: "projects.DEMO.commit_issue_trailer_repos[1] names the repository projects.DEMO.commit_issue_trailer_repos[0] names", takes: asName},
			"two commit_issue_trailer_repos repositories a rename makes one": {body: project("commit_issue_trailer_repos: [acme/"+token.text+", acme/gadgets]") +
				"repository_renames: [{from: acme/" + token.text + ", to: acme/gadgets}]\n",
				place: "projects.DEMO.commit_issue_trailer_repos[0] and projects.DEMO.commit_issue_trailer_repos[1] (acme/gadgets) name one repository", takes: asName},
			"watched_workflows that is no list": {body: project("watched_workflows: " + token.text),
				place: "projects.DEMO.watched_workflows is a string, not a list of entries"},
			"a watched workflow that is no string": {body: project("watched_workflows: [!x " + token.text + "]"),
				place: "projects.DEMO.watched_workflows[0] is a scalar of another tag, not a string"},
			"a watched workflow": {body: project("watched_workflows: [" + token.text + "]"),
				place: "projects.DEMO.watched_workflows[0]"},
			"a watched workflow's form": {body: project("watched_workflows: [acme/" + token.text + "]"),
				place: "projects.DEMO.watched_workflows[0] is not "},
			"a watched workflow's repository": {body: project("watched_workflows: [acme/" + token.text + ".git/x.yaml]"),
				place: "projects.DEMO.watched_workflows[0]: the repository must be ", takes: asName},
			"a watched workflow's repository, the token in its events": {body: project("watched_workflows: ['acme/wid gets/x.yaml@" + token.text + "']"),
				place: "projects.DEMO.watched_workflows[0]: the repository must be "},
			"a watched workflow's file, the token in its events": {body: project("watched_workflows: ['acme/widgets/x y.yaml@" + token.text + "']"),
				place: "projects.DEMO.watched_workflows[0]: the workflow file is not "},
			"a watched workflow's event": {body: project("watched_workflows: ['acme/widgets/x.yaml@" + token.text + "']"),
				place: "projects.DEMO.watched_workflows[0] event 1 has no key's shape "},
			"an empty watched event beside the token": {body: project("watched_workflows: ['acme/widgets/x.yaml@," + token.text + "']"),
				place: `projects.DEMO.watched_workflows[0]: "@" must be followed `},
			"one watched event twice beside the token": {body: project("watched_workflows: ['acme/widgets/x.yaml@push,push," + token.text + "']"),
				place: `projects.DEMO.watched_workflows[0] names the event "push" twice`},
			"one watched workflow twice": {body: project("watched_workflows: [acme/" + token.text + "/x.yaml, acme/" + token.text + "/x.yaml]"),
				place: "projects.DEMO.watched_workflows[0] and projects.DEMO.watched_workflows[1] name one workflow", takes: asName},
			"one watched workflow twice, the token as its file": {body: project("watched_workflows: [acme/widgets/" + token.text + ".yaml, acme/widgets/" + token.text + ".yaml]"),
				place: "projects.DEMO.watched_workflows[0] and projects.DEMO.watched_workflows[1] name one workflow", takes: asFile},
			"repository_renames": {body: minimalFile + "repository_renames: [{from: " + token.text + ", to: acme/widgets}]\n",
				place: "repository_renames[0].from must be "},
			"repository_renames' to": {body: minimalFile + "repository_renames: [{from: acme/widgets, to: " + token.text + "}]\n",
				place: "repository_renames[0].to must be "},
			"repository_renames' from, ending in .git": {body: minimalFile + "repository_renames: [{from: acme/" + token.text + ".git, to: acme/widgets}]\n",
				place: "repository_renames[0].from must be "},
			"repository_renames' to, ending in .git": {body: minimalFile + "repository_renames: [{from: acme/widgets, to: acme/" + token.text + ".git}]\n",
				place: "repository_renames[0].to must be "},
			"a repository renamed to itself": {body: minimalFile + "repository_renames: [{from: acme/" + token.text + ", to: Acme/" + token.text + "}]\n",
				place: "repository_renames: rename 0 (repository_renames[0].from -> repository_renames[0].to) renames a repository to itself", takes: asName},
			"one owner's repositories renamed to two owners": {body: minimalFile + "repository_renames: [{from: " + token.text + "/b, to: c/d}, {from: " + token.text + "/e, to: f/g}]\n",
				place: "move repositories of the owner of repository_renames[1].from to two owners, f and c", takes: asOwner},
			"leak_check.repository": {body: minimalFile + "leak_check: {repository: " + token.text + ", manifest_sha256: " + theManifestSHA256 + "}\n",
				place: "leak_check.repository must be "},
			"leak_check.repository, ending in .git": {body: minimalFile + "leak_check: {repository: acme/" + token.text + ".git, manifest_sha256: " + theManifestSHA256 + "}\n",
				place: "leak_check.repository must be "},
			"leak_check.manifest_sha256": {body: minimalFile + "leak_check: {repository: " + theLeakCheckRepository + ", manifest_sha256: " + token.text + "}\n",
				place: "leak_check.manifest_sha256 must be "},
			"dispatch_url's scheme": {body: strings.Replace(minimalFile, "dispatch_url: http://127.0.0.1:8080", "dispatch_url: "+token.text+"://127.0.0.1:8080", 1),
				place: "dispatch_url"},
			"runtime.tmux.take_over": {body: minimalFile + "runtime: {tmux: {take_over: " + token.text + "}}\n",
				place: "runtime.tmux.take_over at line 18 "},
			"the repository cache's bucket": {body: kubernetesFile + "    repository_cache: {bucket: " + token.text + ", service_account: legion-cache}\n",
				place: "runtime.kubernetes.repository_cache.bucket must be ", kubernetes: true, mixedOnly: true},
			"a pod's service_account": {body: kubernetesFile + "    pod: {service_account: " + token.text + "}\n",
				place: "runtime.kubernetes.pod.service_account is no ", kubernetes: true, mixedOnly: true},
			"a claim's name": {body: pod("      volumes: [{name: share, persistent_volume_claim: {name: " + token.text + "}}]\n"),
				place: "runtime.kubernetes.pod.volumes[0].persistent_volume_claim.name is no ", kubernetes: true, mixedOnly: true},
			"a canary issue": {body: kubernetesFile + "    sandbox: {canary_issues: [" + token.text + "], microvm: {compute_class: example-microvm}}\n",
				place: "runtime.kubernetes.sandbox.canary_issues entry 1 ", kubernetes: true},
			"a metadata workload pool": {body: kubernetesFile + "    sandbox:\n      microvm:\n" + metadataLines(token.text, testIdentityProvider),
				place: "runtime.kubernetes.sandbox.microvm.metadata.workload_pool must be ", kubernetes: true},
			"agent_secrets.url": {body: kubernetesFile + "    agent_secrets:\n      url: http://" + token.text + "\n" + agentSecretsTokenFile,
				place: "runtime.kubernetes.agent_secrets.url must ", kubernetes: true},
			"agent_secrets' audience twice": {body: kubernetesFile + "    agent_secrets:\n      url: https://secrets.internal.example\n" + agentSecretsTokenFile +
				"      audience: " + token.text + "\n      exchange_audience: " + token.text + "\n",
				place: "runtime.kubernetes.agent_secrets.exchange_audience must differ from runtime.kubernetes.agent_secrets.audience (the two are one value)", kubernetes: true},
			"agent_secrets' audience twice, of a Workload Identity pool's form": {body: kubernetesFile + "    agent_secrets:\n      url: https://secrets.internal.example\n" + agentSecretsTokenFile +
				"      audience: " + token.text + ".svc.id.goog\n      exchange_audience: " + token.text + ".svc.id.goog\n",
				place: "runtime.kubernetes.agent_secrets.exchange_audience must differ from runtime.kubernetes.agent_secrets.audience (the two are one value)", kubernetes: true},
			"agent_secrets' audience twice, a writer audience whose pool id is the token": {body: kubernetesFile + "    agent_secrets:\n      url: https://secrets.internal.example\n" + agentSecretsTokenFile +
				"      audience: \"" + writerAudience(token.text, "example-cluster") + "\"\n      exchange_audience: \"" + writerAudience(token.text, "example-cluster") + "\"\n",
				place: "runtime.kubernetes.agent_secrets.exchange_audience must differ from runtime.kubernetes.agent_secrets.audience (the two are one value)", kubernetes: true, takes: asWriterID},
			"agent_secrets' audience twice, a writer audience whose provider id is the token": {body: kubernetesFile + "    agent_secrets:\n      url: https://secrets.internal.example\n" + agentSecretsTokenFile +
				"      audience: \"" + writerAudience("legion-session-writer", token.text) + "\"\n      exchange_audience: \"" + writerAudience("legion-session-writer", token.text) + "\"\n",
				place: "runtime.kubernetes.agent_secrets.exchange_audience must differ from runtime.kubernetes.agent_secrets.audience (the two are one value)", kubernetes: true, takes: asWriterID},
			"a writer audience the broker's audience carries too": {body: writerAudienceBeside(writerAudience(token.text, token.text), writerAudience(token.text, token.text)),
				place: "runtime.kubernetes.session_database.writer_audience must differ from runtime.kubernetes.agent_secrets.audience (the two are one value)", kubernetes: true, takes: asWriterID},
			"a writer audience whose https: form the broker's audience carries": {body: writerAudienceBeside(writerAudience(token.text, token.text), "https:"+writerAudience(token.text, token.text)),
				place: "runtime.kubernetes.session_database.writer_audience must differ from runtime.kubernetes.agent_secrets.audience (its https: form)", kubernetes: true, takes: asWriterID},
			"an NFS export's path": {body: pod("      volumes: [{name: nas, nfs: {server: 10.0.0.2, path: " + token.text + "}}]\n"),
				place: "runtime.kubernetes.pod.volumes[0].nfs.path must be ", kubernetes: true},
			"a mount's path": {body: pod("      volumes: [{name: creds, secret: {name: a}}]\n      volume_mounts: [{volume: creds, mount_path: " + token.text + "}]\n"),
				place: "runtime.kubernetes.pod.volume_mounts[0].mount_path must be ", kubernetes: true},
			"a mount's volume": {body: pod("      volumes: [{name: creds, secret: {name: a}}]\n      volume_mounts: [{volume: " + token.text + ", mount_path: /etc/x}]\n"),
				place: "runtime.kubernetes.pod.volume_mounts[0].volume names no volume ", kubernetes: true},
			"a volume's name twice": {body: pod("      volumes: [{name: " + token.text + ", secret: {name: a}}, {name: " + token.text + ", secret: {name: b}}]\n"),
				place: "runtime.kubernetes.pod.volumes[1].name is the name runtime.kubernetes.pod.volumes[0] carries too", kubernetes: true},
			"a volume's default_mode": {body: pod("      volumes: [{name: creds, secret: {name: a, default_mode: " + token.text + "}}]\n"),
				place: "runtime.kubernetes.pod.volumes[0].secret.default_mode must be ", kubernetes: true},
			"a node selector's label twice": {body: kubernetesFile + "    scheduling: {node_selector: {" + token.text + ": a, " + token.text + ": b}}\n",
				place: "runtime.kubernetes.scheduling.node_selector names the label at line ", kubernetes: true},
		} {
			if reader.mixedOnly && !token.mixed || reader.takes != nil && !reader.takes(token.text) {
				continue
			}
			t.Run(name+"/"+token.shape, func(t *testing.T) {
				load := Load
				if reader.kubernetes {
					load = LoadForValidation
				}
				_, err := load(configFileWith(t, reader.body, "checker.md"), noEnv)
				if err == nil {
					t.Fatalf("the load took the token, want it refused")
				}
				if !strings.Contains(err.Error(), reader.place) {
					t.Fatalf("the refusal %q does not name %q", err, reader.place)
				}
				holdsNoPiece(t, err, token.text)
			})
		}
	}
}

// A token pasted where a file boot reads goes, `instructions` (the daemon's and the controller's)
// or a hook's `prompt_file`, loads, since the loader reads neither file, and the read is refused
// naming the key, never the path nor the read's error, which repeats it; a token pasted where a
// hook's name goes is named by the hook's place, while the path beside it, written in a path's
// shape, is named. It reads the refusal's text for each 8-character piece of the token, as
// TestATokenThatFitsTheRoleGrammarIsNeverPrinted does.
func TestATokenWhereABootReadFileGoesIsNeverPrinted(t *testing.T) {
	hook := func(name, promptFile string) string {
		return minimalFile + "hooks: [{name: " + name + ", on: work_back, every: 1, roles: [implementer], prompt_file: " + promptFile + "}]\n"
	}
	for _, token := range fakeTokens() {
		for name, reader := range map[string]struct {
			read  func(t *testing.T) error
			place string
		}{
			"instructions": {read: func(t *testing.T) error {
				_, err := loadedConfig(t, minimalFile+"instructions: "+token.text+"\n").ReadInstructions()
				return err
			}, place: "instructions names a file that could not be read: it does not exist"},
			"a hook's prompt_file": {read: func(t *testing.T) error {
				_, err := ReadHookPrompt(0, loadedConfig(t, hook("nudge", token.text)).Hooks[0])
				return err
			}, place: "hook nudge prompt_file names a file that could not be read: it does not exist"},
			"a hook's name": {read: func(t *testing.T) error {
				_, err := ReadHookPrompt(0, loadedConfig(t, hook(token.text, "missing.md")).Hooks[0])
				return err
			}, place: "hooks[0].prompt_file /"},
			"the controller's instructions": {read: func(t *testing.T) error {
				cfg, err := LoadController(controllerFile(t, requiredControllerKeys+"instructions: "+token.text+"\n"), "")
				if err != nil {
					t.Fatalf("LoadController: %v, want the file loaded: the loader reads no instructions file", err)
				}
				_, err = cfg.ReadInstructions()
				return err
			}, place: "instructions names a file that could not be read: it does not exist"},
		} {
			t.Run(name+"/"+token.shape, func(t *testing.T) {
				err := reader.read(t)
				if err == nil {
					t.Fatalf("the read took a file no one wrote, want it refused")
				}
				if !strings.Contains(err.Error(), reader.place) {
					t.Fatalf("the refusal %q does not name %q", err, reader.place)
				}
				holdsNoPiece(t, err, token.text)
			})
		}
	}
}

// loadedConfig is the configuration Load reads of body, which the test means to load.
func loadedConfig(t *testing.T, body string) Config {
	t.Helper()
	cfg, err := Load(writeConfigFile(t, body), noEnv)
	if err != nil {
		t.Fatalf("Load: %v, want the file loaded: the loader reads no instructions or hook prompt file", err)
	}
	return cfg
}

// role_config_files and worker_stall_backstop_hours take a role the deployment defines as a key,
// and each check of the key walks it as it walks Legion's roles: its overlays are refused while
// it runs on Claude Code, and its bound below its own stall ladder. A file that defines no such
// role is refused with the text of any other name that is no Legion role, and a name no role
// holds with a list that names the roles of the deployment beside Legion's.
func TestTheRoleKeyedSettingsTakeARoleOfTheDeployment(t *testing.T) {
	cfg, err := Load(rolesFile(t, checkerRole+"role_config_files: {checker: [/etc/x.yml]}\nworker_stall_backstop_hours: {checker: 2}\n", "checker.md"), noEnv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.RoleConfigFiles["checker"], []string{"/etc/x.yml"}; !reflect.DeepEqual(got, want) {
		t.Errorf("RoleConfigFiles[checker] = %v, want %v", got, want)
	}
	if got := cfg.WorkerStallBackstops["checker"]; got != 2*time.Hour {
		t.Errorf("WorkerStallBackstops[checker] = %s, want 2h", got)
	}

	for _, tc := range []struct{ name, body, want string }{
		{"overlays without roles", "role_config_files: {checker: [/etc/x.yml]}\n",
			`role_config_files names "checker", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller)`},
		{"a backstop without roles", "worker_stall_backstop_hours: {checker: 2}\n",
			`worker_stall_backstop_hours names "checker", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller)`},
		{"overlays of a name no role holds", checkerRole + "role_config_files: {ghost: [/etc/x.yml]}\n",
			`role_config_files names "ghost", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller, checker)`},
		{"a backstop of a name no role holds", checkerRole + "worker_stall_backstop_hours: {ghost: 2}\n",
			`worker_stall_backstop_hours names "ghost", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, controller, checker)`},
		{"overlays of a name outside the grammar", checkerRole + "role_config_files: {Checker: [/etc/x.yml]}\n",
			"unknown key at line 19 under role_config_files, whose name has no key's shape (" + keyShapeWords +
				"): it must be a Legion role (architect, planner, implementer, tester, reviewer, merger, controller)"},
		{"overlays of a role on Claude Code", checkerRole + "worker_backends: {checker: claude}\nrole_config_files: {checker: [/etc/x.yml]}\n",
			"role_config_files.checker names Oh My Pi overlays, and worker_backends.checker is claude, which reads none: drop the role from one of the two"},
		{"a backstop below the role's own ladder", checkerRole + "worker_stall_backstop_hours: {checker: 1}\n",
			"worker_stall_backstop_hours.checker (1h0m0s) is below the whole stall ladder the checker's own plugin runs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(rolesFile(t, tc.body, "checker.md"), noEnv)
			if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want %q", err, tc.want)
			}
		})
	}
}

// A project's `memory` overlays reach every role of a memory-group tree, so a role of the
// deployment on Claude Code, which reads no PI_CONFIG_FILES, refuses them as an issue role of
// Legion's on Claude Code does; on Oh My Pi it takes them.
func TestADeploymentRolesOverlaysAreJudgedAsAnIssueRolesAre(t *testing.T) {
	const block = "{mode: on, config_files: [/etc/x.yml]}"
	_, err := Load(configFileWith(t, memoryFile(block, checkerRole+"worker_backends: {checker: claude}\n"), "checker.md"), noEnv)
	want := "projects.DEMO.memory names config_files, and worker_backends.checker is claude, which reads no PI_CONFIG_FILES: put every issue role on omp, or take the overlays out of the block"
	if err == nil || err.Error() != want {
		t.Fatalf("Load with the role on Claude Code = %v, want %q", err, want)
	}
	if _, err := Load(configFileWith(t, memoryFile(block, checkerRole+"worker_backends: {checker: omp}\n"), "checker.md"), noEnv); err != nil {
		t.Fatalf("Load with the role on Oh My Pi: %v", err)
	}
}

// A hook's `roles` and a project's `default_steps` take a role the deployment defines; a file
// that defines no such role is refused with the text of any other name no role holds, which for
// `default_steps` is the plan's own refusal of a role nobody defines, and a name outside the role
// grammar is refused by its place (roleEntryAt). The judgments of `default_steps` run once the
// whole file is read, in the order of the projects' keys, so a refusal of an earlier key comes
// first, and of two projects the one whose key sorts first is told.
func TestAHooksRolesAndDefaultStepsTakeARoleOfTheDeployment(t *testing.T) {
	const hook = "hooks: [{name: nudge, on: work_back, every: 1, roles: [checker], prompt: Look again.}]\n"
	steps := func(projects string) string {
		return strings.Replace(minimalFile, "projects:\n  DEMO: {}\n", "projects:\n"+projects, 1)
	}

	cfg, err := Load(rolesFile(t, checkerRole+hook, "checker.md"), noEnv)
	if err != nil {
		t.Fatalf("Load of a hook of the role: %v", err)
	}
	if got := cfg.Hooks[0].Roles; len(got) != 1 || got[0] != "checker" {
		t.Errorf("Hooks[0].Roles = %v, want [checker]", got)
	}
	cfg, err = Load(configFileWith(t, steps("  DEMO: {default_steps: [implementer, checker]}\n")+checkerRole, "checker.md"), noEnv)
	if err != nil {
		t.Fatalf("Load of default_steps naming the role: %v", err)
	}
	if got := cfg.Projects["DEMO"].DefaultSteps; len(got) != 2 || got[1].Role != "checker" {
		t.Errorf("DefaultSteps = %v, want [implementer checker]", got)
	}

	for _, tc := range []struct{ name, path, want string }{
		{"a hook of the role without roles", rolesFile(t, hook),
			`hooks[0].roles names "checker", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger)`},
		{"a hook of a name no role holds", rolesFile(t, checkerRole+strings.Replace(hook, "[checker]", "[ghost]", 1), "checker.md"),
			`hooks[0].roles names "ghost", which is not a Legion role (architect, planner, implementer, tester, reviewer, merger, checker)`},
		{"a hook of a name outside the grammar", rolesFile(t, checkerRole+strings.Replace(hook, "[checker]", "[Checker]", 1), "checker.md"),
			"hooks[0].roles entry 1 is outside the role grammar: a role's name is " + roleGrammarWords},
		{"default_steps of the role without roles", configFileWith(t, steps("  DEMO: {default_steps: [implementer, checker]}\n")),
			"projects.DEMO.default_steps: step 2 of task default names the role checker, which neither the deployment nor the plan defines."},
		{"default_steps this release refuses for another cause", configFileWith(t, steps("  DEMO: {default_steps: [tester]}\n")),
			"projects.DEMO.default_steps: task default has no step implementer. This release of Legion does not carry it out yet. " +
				"step 1 of task default names tester, and no step before it names the role implementer, whose pull request tester reads; a role of this deployment that reports pass or fail needs none. This release of Legion does not carry it out yet."},
		{"default_steps whose role of the deployment does implementation work after the implementer",
			configFileWith(t, steps("  DEMO: {default_steps: [implementer, checker]}\n")+strings.Replace(checkerRole, "type: testing", "type: implementation", 1), "checker.md"),
			"projects.DEMO.default_steps: task default names checker at step 2, a step of the type implementation after the step implementer at step 1, and Legion's retro is done by the implementer in this release. This release of Legion does not carry it out yet."},
		// A second agent that starts with a role is refused, a role of the deployment as Legion's own.
		{"default_steps that name two agents that start with a role of the deployment",
			configFileWith(t, steps("  DEMO: {default_steps: [implementer, checker, reviewer, checker]}\n")+checkerRole, "checker.md"),
			"projects.DEMO.default_steps: step 4 of task default needs an agent of its own beside the agent of step 2, and both agents start with the role checker, and this release runs one agent that starts with each role for an issue. This release of Legion does not carry it out yet."},
		{"a refusal of an earlier key first", configFileWith(t, steps("  DEMO: {default_steps: [tester]}\n")+"admission_cap: 0\n"),
			"admission_cap must be a positive integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(tc.path, noEnv); err == nil || err.Error() != tc.want {
				t.Fatalf("Load = %v, want %q", err, tc.want)
			}
		})
	}

	t.Run("the project whose key sorts first", func(t *testing.T) {
		path := configFileWith(t, steps("  DEMO: {}\n  BETA: {default_steps: [tester]}\n  ACME: {default_steps: [tester]}\n"))
		for range 20 {
			if _, err := Load(path, noEnv); err == nil || !strings.HasPrefix(err.Error(), "projects.ACME.default_steps: ") {
				t.Fatalf("Load = %v, want the refusal of projects.ACME.default_steps", err)
			}
		}
	})
}
