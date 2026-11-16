package access

import "fmt"

// RepositoryClause passes a GitHub repository, by the owner/name repoExpr names, to the viewer at
// viewerParam when a repo_projects mapping names it, as the mapped repository or as the name it
// was renamed to, for a project the viewer sees (ProjectClause); a repository no mapping names
// passes for an open viewer alone. Names are compared lower-cased, as every comparison of a
// repository is (refs.CanonicalRepoSQL): GitHub spells an owner and a repository as their owners
// do, while a mapping is stored canonicalised.
//
// repoExpr is spliced more than once, so it must be a column or a bound parameter.
func RepositoryClause(repoExpr, viewerParam string) string {
	return fragment("RepositoryClause", fmt.Sprintf(`(%[2]s or case
		when exists (
			select 1 from repo_projects mapping
			where lower(mapping.repo) = lower(%[1]s) or lower(mapping.renamed_to) = lower(%[1]s)
			offset 0)
		then exists (
			select 1 from repo_projects mapping
			where (lower(mapping.repo) = lower(%[1]s) or lower(mapping.renamed_to) = lower(%[1]s))
				and %[4]s
			offset 0)
		else %[3]s end)`,
		repoExpr, viewerFlag(viewerParam, "all"), viewerFlag(viewerParam, "open"), ProjectClause("mapping.project", viewerParam)))
}
