package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// EnsureProjectOwners holds the boot to "every project has an owner" (INFRA-3862 R1). The
// migration that added projects.owner (20261116015736_project_access) gave each project its
// default assignee where that names a person who has signed in, and every project created since
// takes its default assignee as it is inserted (projects_owner_from_default), so a project with
// no owner is one whose default named nobody who had signed in when that migration ran.
//
// fallback is DISPATCH_PROJECT_OWNER_FALLBACK. Where it names a full member's `people` row, every
// project with no owner is given to that person; where it is empty, or names nobody who signed
// in, or names a restricted member, nothing is written, because an owner is always a full member.
// Then it answers an error naming every project still without an owner, with both repairs, and
// the boot refuses to serve.
//
// No lock is needed between the fill and the read: an insert stores an owner whatever it names,
// and no write clears one (projects_owner_kept), so a project can only leave the set the read
// answers, never join it.
func EnsureProjectOwners(ctx context.Context, pool *Pool, fallback string) error {
	if fallback != "" {
		if _, err := pool.Exec(ctx, `
			update projects
			set owner = $1
			where owner is null
			  and exists (select 1 from people where email = $1 and member_kind = 'full')
		`, fallback); err != nil {
			return fmt.Errorf("give the projects with no owner to %s: %w", fallback, err)
		}
	}
	rows, err := pool.Query(ctx, `
		select key || ' (default assignee ' || default_assignee || ')'
		from projects
		where owner is null
		order by key
	`)
	if err != nil {
		return fmt.Errorf("read the projects with no owner: %w", err)
	}
	stranded, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the projects with no owner: %w", err)
	}
	if len(stranded) == 0 {
		return nil
	}
	because := "DISPATCH_PROJECT_OWNER_FALLBACK is not set"
	if fallback != "" {
		because = fmt.Sprintf("DISPATCH_PROJECT_OWNER_FALLBACK names %s, who is not a full member who has signed in", fallback)
	}
	return fmt.Errorf("%d project(s) have no owner, because each one's default assignee never signed in, and %s: %s. "+
		"Set DISPATCH_PROJECT_OWNER_FALLBACK to the email of a full member who has signed in, and that person becomes "+
		"their owner at the next boot; or give each one an owner with `envoy-dispatch project-owner <KEY> <email>`",
		len(stranded), because, strings.Join(stranded, ", "))
}
