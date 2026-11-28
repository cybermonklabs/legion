package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// repositoryRenamesKey is `repository_renames`, the key every refusal of it names.
const repositoryRenamesKey = "repository_renames"

// repositoryShape is a repository written in the characters GitHub allows, ghrepo's owner and
// name joined by one "/": an owner of a letter or a digit and then at most 38 letters, digits, "-"
// and "_", and a name of at most 100 letters, digits, "-", "_" and ".".
var repositoryShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// parseRepository is ghrepo.Parse for repository, a value the file or the environment writes at
// what, refused by what alone where it has neither a repository's shape (repositoryShape) nor a
// key's (keyShape): Parse's refusal quotes the value whole, and a value of neither shape, a token
// pasted where the repository goes among them (`ghp_…` holds no "/"), is no typo of one. A value
// of either shape is Parse's to refuse in its own words, which name it: a bare word such as
// `widgets`, or a name ending in `.git`, whose refusal says how to write it.
func parseRepository(what, repository string) (ghrepo.Repository, error) {
	if !repositoryShape.MatchString(repository) && !keyShape(repository) {
		return ghrepo.Repository{}, fmt.Errorf(`%s must be "owner/name": an owner of at most 39 ASCII letters, digits, "-" and "_", beginning with a letter or digit, and a name of at most 100 ASCII letters, digits, "-", "_" and "."`, what)
	}
	return ghrepo.Parse(what, repository)
}

// readRepositoryRenames is `repository_renames`: a list of mappings, each exactly a `from` and a
// `to` Parse takes (parseRepository), the repository as it was and the one it moved to. It is a
// top-level key rather than a project's, because the equivalence serves the intake filters and a
// binary-wide comparison, neither of which is per project. An entry that is not a mapping, a field
// that is not one of the two, a missing or empty side and a side Parse refuses are each refused
// naming the entry; the structural rules over the whole list are ghrepo.NewRenames'
// (checkRepositoryRenames), once the list is read. An absent key, a null one and an empty list are
// each no rename.
func readRepositoryRenames(value *yaml.Node, key string) ([]ghrepo.Rename, error) {
	if value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s must be an array", key)
	}
	var renames []ghrepo.Rename
	for index, entry := range value.Content {
		field := fmt.Sprintf("%s[%d]", key, index)
		if entry.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping", field)
		}
		fields, err := describedMembers(entry, field, repositoryRenamesKey+"[]")
		if err != nil {
			return nil, err
		}
		from, err := requiredString(fields["from"], field+".from", ": the repository as it was")
		if err != nil {
			return nil, err
		}
		to, err := requiredString(fields["to"], field+".to", ": the repository it moved to")
		if err != nil {
			return nil, err
		}
		var rename ghrepo.Rename
		if rename.From, err = parseRepository(field+".from", from); err != nil {
			return nil, err
		}
		if rename.To, err = parseRepository(field+".to", to); err != nil {
			return nil, err
		}
		renames = append(renames, rename)
	}
	return renames, nil
}

// checkRepositoryRenames settles the list `repository_renames` read into the one equivalence every
// consumer takes (ghrepo.Renames), refusing what that value cannot hold — a repository renamed to
// itself, one renamed twice, a chain of two pairs, one owner's repositories moved to two owners —
// with the key before the pair ghrepo names. A file that configures none settles the zero value,
// which answers exactly as strings.EqualFold does.
func checkRepositoryRenames(file fileConfig, cfg *Config) error {
	renames, err := ghrepo.NewRenames(file.RepositoryRenames)
	if err != nil {
		return fmt.Errorf("%s: %w", repositoryRenamesKey, err)
	}
	cfg.RepositoryRenames = renames
	return nil
}

// checkCommitIssueTrailerRenames refuses two `commit_issue_trailer_repos` entries of one project
// whose repositories a configured rename makes one repository. readCommitIssueTrailerRepos
// already refuses one name twice (compared without case), but it reads a project before the
// renames settle and knows nothing of them, so the pair is caught here, where both the
// equivalence and every project's list stand. A rename is read wherever a name is compared
// (ghrepo.Renames.Same), so the two entries select the one repository, and the list would name
// that repository twice while saying nothing a single entry does not.
func checkCommitIssueTrailerRenames(cfg *Config) error {
	if cfg.RepositoryRenames.IsZero() {
		return nil
	}
	for _, key := range slices.Sorted(maps.Keys(cfg.Projects)) {
		repos := cfg.Projects[key].CommitIssueTrailerRepos
		for index, repo := range repos {
			for earlier, named := range repos[:index] {
				if !cfg.RepositoryRenames.Same(named.String(), repo.String()) {
					continue
				}
				return fmt.Errorf("projects.%s.commit_issue_trailer_repos[%d] (%s) and projects.%s.commit_issue_trailer_repos[%d] (%s) name one repository: %s makes %s and %s one repository, so the list names it twice; name it once",
					key, earlier, named, key, index, repo, repositoryRenamesKey, named, repo)
			}
		}
	}
	return nil
}
