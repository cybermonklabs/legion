package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// The broker keeps the last answer Dispatch's project route gave it for each person and project in
// 0049's project_standing, so that while Dispatch cannot be asked the request machine's person rule
// answers from it (projectaccess.OrKept): a project Dispatch last answered public or visible to the
// person shows them the row, until an answer for anyone says the project is private since, and
// every other answer, or none, refuses as before. Every process writes the answers its own asks
// fetched, and the row keeps the one asked latest by the clock of the process that asked, since
// Dispatch's project answer names no time of its own.
//
// The table is bounded by what Dispatch holds, and not by what a caller can name. An answer that
// Dispatch holds no such project is kept for any project key a caller names, and every
// issue-key-shaped word of a request's reason is one, so those rows alone are capped
// (MaxKeptMissingProjects); every other row is an answer about a project Dispatch holds, and goes
// when Dispatch answers that it holds the project no more.

// MaxKeptMissingProjects is how many answers that Dispatch holds no such project the broker keeps.
// A write past it ends the oldest asked (KeepProjectStanding), so a read while Dispatch cannot be
// asked refuses that key as one never asked, which fails closed. Nothing ends a kept answer by
// its age: only a write ends one, and a write follows an answer of Dispatch, so none ends during
// an outage. A key keeps its answer while fewer than MaxKeptMissingProjects other keys were
// answered no project after it was last asked.
const MaxKeptMissingProjects = 10_000

// keepProjectStandingSQL writes an answer unless the row holds one asked later.
const keepProjectStandingSQL = `insert into project_standing (person, project, visibility, visible, asked_at) values ($1, $2, nullif($3, ''), $4, $5)
	on conflict (person, project) do update set visibility = excluded.visibility, visible = excluded.visible, asked_at = excluded.asked_at
	where project_standing.asked_at < excluded.asked_at`

// KeepProjectStanding writes Dispatch's answer for person and project, asked at askedAt, unless the
// row already holds an answer asked later: visibility as Dispatch answered it, "public" or
// "private", and whether person may see the project; or, for visibility "", that Dispatch holds no
// such project, which is written for no person (the empty person), since it is the same answer for
// everyone. person is a login as record.CanonicalLogin spells it, or empty for a question naming
// no person.
//
// An answer that Dispatch holds no such project also, in the same transaction, deletes every
// person's answer for that project asked before it, and then ends the oldest asked no-project
// answers past MaxKeptMissingProjects. A person's answer asked before Dispatch said it holds no
// such project is about a project deleted since: KeptProjectStanding never serves it beside the
// later no-project answer, and once deleted it is not served either when that answer is gone,
// ended past the cap or replaced by a later answer for no person that a project made again under
// the same key is private. A no-project answer is not ended past the cap while a person's answer
// of its project asked before it is kept, which only a slower process's write after it leaves:
// it is what hides that answer. Every no-project write takes one transaction lock
// (keepMissingProjectLock) before it locks a row, so no two wait on each other's rows.
func (s *Store) KeepProjectStanding(ctx context.Context, person, project, visibility string, visible bool, askedAt time.Time) error {
	if visibility != "" {
		_, err := s.Pool.Exec(ctx, keepProjectStandingSQL, person, project, visibility, visible, askedAt)
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, keepMissingProjectLock); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, keepProjectStandingSQL, "", project, "", false, askedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `delete from project_standing where project = $1 and person <> '' and asked_at < $2`, project, askedAt); err != nil {
			return err
		}
		// beyond is read once (materialized): planned as a subquery, a table with no statistics
		// yet ran it again for each no-project row, ten thousand scans of ten thousand rows.
		_, err := tx.Exec(ctx, `with beyond as materialized (
				select project from project_standing where person = '' and visibility is null
				order by asked_at desc, project desc offset $1)
			delete from project_standing missing using beyond
			where missing.project = beyond.project and missing.person = '' and missing.visibility is null
				and not exists (select 1 from project_standing hidden
					where hidden.project = missing.project and hidden.person <> '' and hidden.asked_at < missing.asked_at)`,
			MaxKeptMissingProjects)
		return err
	})
}

// keepMissingProjectLock names the transaction lock every write of an answer that Dispatch holds no
// such project takes first (KeepProjectStanding).
const keepMissingProjectLock = "project-standing:missing"

// ProjectStanding is one answer of Dispatch's project route as the broker keeps it.
type ProjectStanding struct {
	// Visibility is "public" or "private" as Dispatch answered it, or "" for an answer that
	// Dispatch holds no such project.
	Visibility string
	// Visible is whether the person the answer was asked for may see the project.
	Visible bool
	// AskedAt is when the broker asked, read before the question was sent.
	AskedAt time.Time
}

// KeptProjectStanding is the last answer kept for person and project: the later, by AskedAt, of
// the answer kept for that person and an answer that Dispatch holds no such project, which is
// kept for no person. Whether the project exists and whether it is public are facts of the
// project, the same for every person, so that last answer stands only while no answer kept for
// any person, no person included, and asked at or after it, says otherwise: a no-project answer
// until an answer that the project exists, and a public answer until an answer that it is
// private. Once one does, the project was made, or made private, since, and a person with no
// answer of their own asked after it is kept nothing, so a read while Dispatch cannot be asked
// refuses them as one never asked rather than show them the project's rows on the older fact. A
// private answer is not ended by a later public one: whether the person may see a private
// project rests on its owner and its access list, which a switch to public takes from nobody.
// found is false when no answer is kept, which is no error.
func (s *Store) KeptProjectStanding(ctx context.Context, person, project string) (standing ProjectStanding, found bool, err error) {
	err = s.Pool.QueryRow(ctx, `select coalesce(last.visibility, ''), last.visible, last.asked_at from (
			select visibility, visible, asked_at from project_standing
			where project = $2 and (person = $1 or (person = '' and visibility is null))
			order by asked_at desc limit 1) last
		where not exists (
			select 1 from project_standing later
			where later.project = $2 and later.asked_at >= last.asked_at
				and case when last.visibility is null then later.visibility is not null
					when last.visibility = 'public' then later.visibility = 'private'
					else false end)`, person, project).Scan(&standing.Visibility, &standing.Visible, &standing.AskedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectStanding{}, false, nil
	}
	if err != nil {
		return ProjectStanding{}, false, err
	}
	return standing, true, nil
}
