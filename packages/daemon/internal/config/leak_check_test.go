package config

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// The two values of a deployment's shared leak check as these tests write them: a neutral
// repository, and a checker sha256 of the shape `sha256sum` prints and another one, so a test reads
// which source a value came from.
const (
	theLeakCheckRepository = "example-org/deployment"
	theCheckerSHA256       = "5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"
	otherCheckerSHA256     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// deploymentDaemonEnvironment is the leak check's part of the environment a deployment's own
// program writes into every daemon container, spelled as that program spells it: the two names
// are literals here and never leakguard's constants, so a rename of either constant fails
// TestTheDeploymentsDaemonEnvironmentNamesTheSetting rather than leaving every daemon the
// deployment runs with no setting and no refusal.
var deploymentDaemonEnvironment = map[string]string{
	"LEGION_LEAK_CHECK_REPOSITORY": theLeakCheckRepository,
	"LEGION_LEAK_CHECK_SHA256":     theCheckerSHA256,
}

// A daemon handed deploymentDaemonEnvironment, and nothing in its file, reads both values: the
// setting the deployment wrote is the one it runs. A name either side spells otherwise leaves one
// value unread, and the load is then refused or reads no setting.
func TestTheDeploymentsDaemonEnvironmentNamesTheSetting(t *testing.T) {
	cfg, err := Load(writeConfigFile(t, minimalFile), envMap(deploymentDaemonEnvironment))
	if err != nil {
		t.Fatalf("load with the deployment's daemon environment: %v", err)
	}
	want := leakguard.Setting{Repository: ghrepo.MustParse(theLeakCheckRepository), CheckerSHA256: theCheckerSHA256}
	if cfg.LeakCheck != want || !cfg.LeakCheck.Configured() {
		t.Fatalf("the setting = %+v, want %+v, read from the deployment's two variables", cfg.LeakCheck, want)
	}
}

// The setting is read from the file or from the daemon's environment, and the file overrides the
// environment key by key, as postgres_dsn overrides LEGION_POSTGRES_DSN: a file that names one
// member takes the other from the environment. Neither anywhere is a deployment that runs no
// shared leak check, which loads.
func TestTheLeakCheckSettingIsTheFileOverTheEnvironmentKeyByKey(t *testing.T) {
	want := leakguard.Setting{Repository: ghrepo.MustParse(theLeakCheckRepository), CheckerSHA256: theCheckerSHA256}
	for _, tc := range []struct {
		name  string
		block string
		env   map[string]string
		want  leakguard.Setting
	}{
		{name: "neither", want: leakguard.Setting{}},
		{name: "the file alone", block: "leak_check:\n  repository: " + theLeakCheckRepository + "\n  checker_sha256: " + theCheckerSHA256 + "\n", want: want},
		{name: "the environment alone", env: map[string]string{
			leakguard.RepositoryVar: theLeakCheckRepository, leakguard.SHA256Var: theCheckerSHA256,
		}, want: want},
		{name: "the file's checker over the environment's", block: "leak_check: {checker_sha256: " + theCheckerSHA256 + "}\n", env: map[string]string{
			leakguard.RepositoryVar: theLeakCheckRepository, leakguard.SHA256Var: otherCheckerSHA256,
		}, want: want},
		{name: "the file's repository over the environment's", block: "leak_check: {repository: " + theLeakCheckRepository + "}\n", env: map[string]string{
			leakguard.RepositoryVar: "acme/elsewhere", leakguard.SHA256Var: theCheckerSHA256,
		}, want: want},
		{name: "a null block, which is no block", block: "leak_check:\n", env: map[string]string{
			leakguard.RepositoryVar: theLeakCheckRepository, leakguard.SHA256Var: theCheckerSHA256,
		}, want: want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfigFile(t, minimalFile+tc.block), envMap(tc.env))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.LeakCheck != tc.want || cfg.LeakCheck.Configured() != tc.want.Configured() {
				t.Fatalf("the setting = %+v (configured %t), want %+v (configured %t)",
					cfg.LeakCheck, cfg.LeakCheck.Configured(), tc.want, tc.want.Configured())
			}
		})
	}
}

// One value without the other is refused at load, naming the half that is set and the two places
// the missing half may come from: whichever source the one value came from, the half that is
// missing is no setting the daemon can run. A value the file names does not stand in for the half
// the environment lacks.
func TestOneLeakCheckValueWithoutTheOtherIsRefused(t *testing.T) {
	const together = "; the repository and the checker sha256 of the deployment's leak check are set together or not at all"
	const missingChecker = " is set and the checker sha256 is not: set LEGION_LEAK_CHECK_SHA256, or leak_check.checker_sha256 in legion.yaml" + together
	const missingRepository = " is set and the repository is not: set LEGION_LEAK_CHECK_REPOSITORY, or leak_check.repository in legion.yaml" + together
	for _, tc := range []struct {
		name  string
		block string
		env   map[string]string
		want  string
	}{
		{name: "the file's repository alone", block: "leak_check: {repository: " + theLeakCheckRepository + "}\n",
			want: "leak_check.repository" + missingChecker},
		{name: "the file's checker alone", block: "leak_check: {checker_sha256: " + theCheckerSHA256 + "}\n",
			want: "leak_check.checker_sha256" + missingRepository},
		{name: "the environment's repository alone", env: map[string]string{leakguard.RepositoryVar: theLeakCheckRepository},
			want: "LEGION_LEAK_CHECK_REPOSITORY" + missingChecker},
		{name: "the environment's checker alone", env: map[string]string{leakguard.SHA256Var: theCheckerSHA256},
			want: "LEGION_LEAK_CHECK_SHA256" + missingRepository},
		{name: "the file's repository and an empty environment checker", block: "leak_check: {repository: " + theLeakCheckRepository + "}\n",
			env: map[string]string{leakguard.SHA256Var: ""}, want: "leak_check.repository" + missingChecker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+tc.block), envMap(tc.env))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("load = %v, want %q", err, tc.want)
			}
		})
	}
}

// A malformed value is refused naming where it came from: a repository that is no owner/name, a
// checker that is not 64 lowercase hex digits — one of capitals, one a digit short, one that is no
// hex at all — an empty member, a member the block does not have, and a block that is no mapping.
func TestAMalformedLeakCheckValueIsRefused(t *testing.T) {
	both := func(repository, sha string) string {
		return "leak_check:\n  repository: " + repository + "\n  checker_sha256: " + sha + "\n"
	}
	for _, tc := range []struct {
		name  string
		block string
		env   map[string]string
		says  string
	}{
		{name: "a repository that is no owner/name", block: both("deployment", theCheckerSHA256),
			says: `leak_check.repository must be "owner/name" (got "deployment")`},
		{name: "a repository of three parts", block: both("example-org/deployment/leak-check", theCheckerSHA256),
			says: `leak_check.repository must be "owner/name"`},
		{name: "a checker in capitals", block: both(theLeakCheckRepository, strings.ToUpper(theCheckerSHA256)),
			says: "leak_check.checker_sha256 must be the sha256 of the check's public_leak_check.py, 64 lowercase hex digits"},
		{name: "a checker a digit short", block: both(theLeakCheckRepository, theCheckerSHA256[1:]),
			says: "leak_check.checker_sha256 must be the sha256 of the check's public_leak_check.py, 64 lowercase hex digits"},
		{name: "a checker that is no hex", block: both(theLeakCheckRepository, strings.Repeat("g", 64)),
			says: "leak_check.checker_sha256 must be the sha256 of the check's public_leak_check.py, 64 lowercase hex digits"},
		{name: "the environment's malformed repository", env: map[string]string{
			leakguard.RepositoryVar: "example-org", leakguard.SHA256Var: theCheckerSHA256,
		}, says: `LEGION_LEAK_CHECK_REPOSITORY must be "owner/name" (got "example-org")`},
		{name: "the environment's malformed checker", env: map[string]string{
			leakguard.RepositoryVar: theLeakCheckRepository, leakguard.SHA256Var: "sha256:" + theCheckerSHA256,
		}, says: "LEGION_LEAK_CHECK_SHA256 must be the sha256 of the check's public_leak_check.py, 64 lowercase hex digits"},
		{name: "an empty member", block: "leak_check: {repository: \"\", checker_sha256: " + theCheckerSHA256 + "}\n",
			says: "leak_check.repository must not be empty"},
		{name: "a member the block does not have", block: both(theLeakCheckRepository, theCheckerSHA256) + "  tag: leak-check-v15\n",
			says: "unknown key leak_check.tag"},
		{name: "a block that is no mapping", block: "leak_check: " + theLeakCheckRepository + "\n",
			says: "leak_check must be a mapping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfigFile(t, minimalFile+tc.block), envMap(tc.env))
			if err == nil || !strings.HasPrefix(err.Error(), tc.says) {
				t.Fatalf("load = %v, want a refusal beginning %q", err, tc.says)
			}
		})
	}
}
