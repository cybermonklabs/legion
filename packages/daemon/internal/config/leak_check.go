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
	leakCheckManifestKey   = leakCheckKey + ".manifest_sha256"
)

// manifestSHA256 is the shape of `leak_check.manifest_sha256`: a sha256 in lowercase hex, which is
// how `sha256sum` prints one.
var manifestSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// fileLeakCheck is `leak_check` as the file wrote it: each member a pointer, so a member the file
// leaves out is told from one it sets and leaves the environment's value standing
// (resolveLeakCheck).
type fileLeakCheck struct {
	Repository     *string
	ManifestSHA256 *string
}

// readLeakCheck is `leak_check`: a mapping of `repository` and `manifest_sha256`, each a non-empty
// string, refused here where its key is known. Whether the two stand together, and each one's
// shape (an owner/name ghrepo.Parse takes, 64 lowercase hex digits), are settled once the
// environment's values are read (resolveLeakCheck). An absent key and a null one are no block.
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
	if block.ManifestSHA256, err = readNonEmptyString(fields["manifest_sha256"], key+".manifest_sha256"); err != nil {
		return nil, err
	}
	return block, nil
}

// resolveLeakCheck settles the deployment's shared public leak check (Config.LeakCheck): each of
// the two values from the daemon's environment (leakguard.RepositoryVar, leakguard.ManifestVar),
// which the file's member of the same value overrides, as postgres_dsn overrides
// LEGION_POSTGRES_DSN. The repository must be one ghrepo.Parse takes and the manifest digest
// (leakguard.Manifest) 64 lowercase hex digits, each refused naming where it came from. The two are
// set together or not at all: one without the other is refused naming the half that is set and the
// two places the missing half may come from, since a check read from a repository with no digest
// to hold it to, or a digest with no repository to read the check from, is a half of the setting
// the daemon cannot run. With neither, the deployment runs no shared leak check, which is no
// refusal (leakguard.Setting.Configured).
func resolveLeakCheck(file fileConfig, env func(string) string, cfg *Config) error {
	repository, repositoryFrom := env(leakguard.RepositoryVar), leakguard.RepositoryVar
	sha, shaFrom := env(leakguard.ManifestVar), leakguard.ManifestVar
	if block := file.LeakCheck; block != nil {
		if block.Repository != nil {
			repository, repositoryFrom = *block.Repository, leakCheckRepositoryKey
		}
		if block.ManifestSHA256 != nil {
			sha, shaFrom = *block.ManifestSHA256, leakCheckManifestKey
		}
	}
	const together = "the repository and the manifest sha256 of the deployment's leak check are set together or not at all"
	switch {
	case repository == "" && sha == "":
		return nil
	case sha == "":
		return fmt.Errorf("%s is set and the manifest sha256 is not: set %s, or %s in legion.yaml; %s",
			repositoryFrom, leakguard.ManifestVar, leakCheckManifestKey, together)
	case repository == "":
		return fmt.Errorf("%s is set and the repository is not: set %s, or %s in legion.yaml; %s",
			shaFrom, leakguard.RepositoryVar, leakCheckRepositoryKey, together)
	}
	parsed, err := ghrepo.Parse(repositoryFrom, repository)
	if err != nil {
		return err
	}
	if !manifestSHA256.MatchString(sha) {
		return fmt.Errorf("%s must be the check's manifest digest, 64 lowercase hex digits (got %+q)", shaFrom, sha)
	}
	cfg.LeakCheck = leakguard.Setting{Repository: parsed, ManifestSHA256: sha}
	return nil
}
