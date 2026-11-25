package contracts

import "regexp"

// projectKey is ProjectKeyPattern, compiled once for every Go reader of a project key.
var projectKey = regexp.MustCompile(ProjectKeyPattern)

// ValidProjectKey reports whether key is a whole project key as ProjectKeyPattern spells one;
// ProjectKeyRule says the rule in words for a refusal.
func ValidProjectKey(key string) bool {
	return projectKey.MatchString(key)
}
