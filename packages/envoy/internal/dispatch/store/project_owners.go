package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// OwnerGiver gives project key to owner in tx, when the project still has no owner under its owner
// lock, and records the grant as an operator's grant is recorded: access.GiveOwnerless, which this
// package cannot import, since access imports it.
type OwnerGiver func(ctx context.Context, tx pgx.Tx, key, owner string) error

// EnsureProjectOwners holds the boot to "every project has an owner" (R1). The migration that added
// projects.owner (20261116015736_project_access) gave each project its default assignee as owner
// where a `people` row holds that address, and an insert that names no owner takes its default
// assignee as it is inserted (projects_owner_from_default), whoever that names. So a project with no
// owner is one whose default named nobody who had signed in when that migration ran.
//
// fallback is DISPATCH_PROJECT_OWNER_FALLBACK. Where it names a full member's `people` row, give
// gives every project with no owner to that person, each in a transaction of its own; where it is
// empty, or names nobody who signed in, or names a restricted member, nothing is written, because
// the fallback names a full member. Then it answers an error naming every project still without an
// owner, by its key, with both repairs, and the boot refuses to serve. The error names no person's
// email, neither a default assignee's nor the fallback's, because the boot writes it to its log.
//
// An insert stores an owner whatever it names, and no write clears one (projects_owner_kept), so a
// project can only leave the set the read answers, never join it; give reads it again under the
// project's lock.
func EnsureProjectOwners(ctx context.Context, pool *Pool, fallback string, give OwnerGiver) error {
	if fallback != "" {
		var full bool
		if err := pool.QueryRow(ctx, `select exists (select 1 from people where email = $1 and member_kind = 'full')`, fallback).Scan(&full); err != nil {
			return fmt.Errorf("read whether DISPATCH_PROJECT_OWNER_FALLBACK names a full member: %w", err)
		}
		if full {
			ownerless, err := ownerlessProjects(ctx, pool)
			if err != nil {
				return err
			}
			for _, key := range ownerless {
				if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return give(ctx, tx, key, fallback) }); err != nil {
					return fmt.Errorf("give project %s to the person DISPATCH_PROJECT_OWNER_FALLBACK names: %w", key, err)
				}
			}
		}
	}
	stranded, err := ownerlessProjects(ctx, pool)
	if err != nil {
		return err
	}
	if len(stranded) == 0 {
		return nil
	}
	because := "DISPATCH_PROJECT_OWNER_FALLBACK is not set"
	if fallback != "" {
		because = "DISPATCH_PROJECT_OWNER_FALLBACK names no full member who has signed in"
	}
	return fmt.Errorf("%d project(s) have no owner, because each one's default assignee never signed in, and %s: %s. "+
		"Set DISPATCH_PROJECT_OWNER_FALLBACK to the email of a full member who has signed in, and that person becomes "+
		"their owner at the next boot; or give each one an owner with `envoy-dispatch project-owner <KEY> <email>`",
		len(stranded), because, strings.Join(stranded, ", "))
}

// ownerlessProjects is the key of every project with no owner, sorted.
func ownerlessProjects(ctx context.Context, pool *Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `select key from projects where owner is null order by key`)
	if err != nil {
		return nil, fmt.Errorf("read the projects with no owner: %w", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the projects with no owner: %w", err)
	}
	return keys, nil
}
