package config

import (
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// leakCheckKey is `leak_check`, the key every refusal of it names, and the nestedKeys group of its
// two members.
const leakCheckKey = "leak_check"

// The two members of `leak_check`, each with the environment variable it overrides.
const (
	leakCheckRepositoryKey = leakCheckKey + ".repository"
	leakCheckSHA256Key     = leakCheckKey + ".checker_sha256"
)

// checkerSHA256 is the shape of `leak_check.checker_sha256`: a sha256 in lowercase hex, which is
// how `sha256sum` prints one.
var checkerSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// fileLeakCheck is `leak_check` as the file wrote it: each member a pointer, so a member the file
// leaves out is told from one it sets and leaves the environment's value standing
// (resolveLeakCheck).
type fileLeakCheck struct {
	Repository    *string
	CheckerSHA256 *string
}

// readLeakCheck is `leak_check`: a mapping of `repository`, an owner/name ghrepo.Parse takes, and
// `checker_sha256`, 64 lowercase hex digits. Each member's shape is refused here, where its key is
// known; whether the two stand together is settled once the environment's values are read
// (resolveLeakCheck). An absent key and a null one are no block.
func readLeakCheck(value *yaml.Node, key string) (*fileLeakCheck, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", key)
	}
	fields, err := describedMembers(value, key, leakCheckKey)
	if err != nil {
		return nil, err
	}
	block := &fileLeakCheck{}
	if block.Repository, err = readNonEmptyString(fields["repository"], key+".repository"); err != nil {
		return nil, err
	}
	if block.CheckerSHA256, err = readNonEmptyString(fields["checker_sha256"], key+".checker_sha256"); err != nil {
		return nil, err
	}
	return block, nil
}

// resolveLeakCheck settles the deployment's shared public leak check (Config.LeakCheck): each of
// the two values from the daemon's environment (leakguard.RepositoryVar, leakguard.SHA256Var),
// which the file's member of the same value overrides, as postgres_dsn overrides
// LEGION_POSTGRES_DSN. The repository must be one ghrepo.Parse takes and the sha256 64 lowercase
// hex digits, each refused naming where it came from. The two are set together or not at all: one
// without the other is refused naming the half that is set and the two places the missing half
// may come from, since a check read from a repository with no checker to hold it to, or a checker
// with no repository to read the check from, is a half of the setting the daemon cannot run. With
// neither, the deployment runs no shared leak check, which is no refusal
// (leakguard.Setting.Configured).
func resolveLeakCheck(file fileConfig, env func(string) string, cfg *Config) error {
	repository, repositoryFrom := env(leakguard.RepositoryVar), leakguard.RepositoryVar
	sha, shaFrom := env(leakguard.SHA256Var), leakguard.SHA256Var
	if block := file.LeakCheck; block != nil {
		if block.Repository != nil {
			repository, repositoryFrom = *block.Repository, leakCheckRepositoryKey
		}
		if block.CheckerSHA256 != nil {
			sha, shaFrom = *block.CheckerSHA256, leakCheckSHA256Key
		}
	}
	const together = "the repository and the checker sha256 of the deployment's leak check are set together or not at all"
	switch {
	case repository == "" && sha == "":
		return nil
	case sha == "":
		return fmt.Errorf("%s is set and the checker sha256 is not: set %s, or %s in legion.yaml; %s",
			repositoryFrom, leakguard.SHA256Var, leakCheckSHA256Key, together)
	case repository == "":
		return fmt.Errorf("%s is set and the repository is not: set %s, or %s in legion.yaml; %s",
			shaFrom, leakguard.RepositoryVar, leakCheckRepositoryKey, together)
	}
	parsed, err := ghrepo.Parse(repositoryFrom, repository)
	if err != nil {
		return err
	}
	if !checkerSHA256.MatchString(sha) {
		return fmt.Errorf("%s must be the sha256 of the check's public_leak_check.py, 64 lowercase hex digits (got %+q)", shaFrom, sha)
	}
	cfg.LeakCheck = leakguard.Setting{Repository: parsed, CheckerSHA256: sha}
	return nil
}
