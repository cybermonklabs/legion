package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A GitHub subject spells a repository lossily: contracts.GithubRepositoryPrefix writes each `.`,
// whitespace, `*` and `>` of the owner and of the name as `_`, so acme/a.b and acme/a_b publish on
// one lane, notifications.github.acme.a_b, and a grant for either carries the other's events. So no
// mapping of a private project shares a lane with a mapping of another project (refuseSharedLane),
// whichever of the two is private. Two public projects may share one, as they always could, so a
// deployment with no private project answers every mapping write as it did. The writers that add
// a lane hold the rule: the mapping PUT, the boot seed, and the switch of a project to private.
// The DELETE and the default demotion add none.
//
// A mapping is a row of its project's: a mapping of a project the caller cannot see is neither
// moved, removed nor named to it (refuseHiddenMapping), and a write into such a project is
// answered as one into a project no row holds.

// codeRepositoryLaneShared is the refusal of a write that would put a private project's
// repository on another project's GitHub subject lane: 409.
const codeRepositoryLaneShared = "REPOSITORY_LANE_SHARED"

// laneRule ends every REPOSITORY_LANE_SHARED answer.
const laneRule = "a private project's repository cannot share a lane with another project's repository"

// repoLanes answers the GitHub subject lanes a mapping's events publish on: one for the stored
// repo, and one for renamedTo when it is set, since the listener publishes a renamed
// repository's events under both names. Each is contracts.GithubRepositoryPrefix of the name's
// two halves, split at its first `/`, with its case folded: every API write stores lowercase
// (canonicalRepo), and the fold reads a row written by direct SQL as the listener compares it,
// without case (ghrename.Parse).
func repoLanes(repo string, renamedTo *string) []string {
	names := []string{repo}
	if renamedTo != nil {
		names = append(names, *renamedTo)
	}
	lanes := make([]string, len(names))
	for index, name := range names {
		owner, repository, _ := strings.Cut(name, "/")
		lanes[index] = strings.ToLower(contracts.GithubRepositoryPrefix(owner, repository))
	}
	return lanes
}

// repoProjectRow is one mapping as refuseSharedLane reads it: its repository, the name it was
// renamed to, its project, and whether that project is private.
type repoProjectRow struct {
	repo      string
	renamedTo *string
	project   string
	private   bool
}

// refuseSharedLane refuses the write in tx, 409 REPOSITORY_LANE_SHARED (sharedLaneError), when a
// lane of a mapping checked selects is also a lane of a mapping of a different project and either
// project is private. It reads every mapping as the write leaves them, so the written row never
// meets itself, and a private project's own mappings never refuse each other.
//
// It first takes one lock over every lane, as refuseRepoRenameChain does over the renames. It is
// each writer's last lock: every caller already holds its owner-row locks (and the PUT the rename
// lock), and none takes another lock after this one, so no two writers wait on each other in a
// circle. The read is a statement of its own after the lock, so under READ COMMITTED the second
// of two writers reads the first's commit.
func refuseSharedLane(ctx context.Context, tx pgx.Tx, viewer *model.Viewer, checked func(repoProjectRow) bool) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('dispatch-repo-lane'), 0)`); err != nil {
		return fmt.Errorf("lock the repository lanes: %w", err)
	}
	rows, err := tx.Query(ctx, `
		select r.repo, r.renamed_to, r.project, p.visibility = 'private'
		from repo_projects r join projects p on p.key = r.project
		order by r.repo
	`)
	if err != nil {
		return err
	}
	mappings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (repoProjectRow, error) {
		var mapping repoProjectRow
		return mapping, row.Scan(&mapping.repo, &mapping.renamedTo, &mapping.project, &mapping.private)
	})
	if err != nil {
		return err
	}
	holders := map[string][]repoProjectRow{}
	for _, mapping := range mappings {
		for _, lane := range repoLanes(mapping.repo, mapping.renamedTo) {
			holders[lane] = append(holders[lane], mapping)
		}
	}
	for _, mapping := range mappings {
		if !checked(mapping) {
			continue
		}
		for _, lane := range repoLanes(mapping.repo, mapping.renamedTo) {
			for _, other := range holders[lane] {
				if other.project != mapping.project && (mapping.private || other.private) {
					return sharedLaneError(viewer, lane, mapping, other)
				}
			}
		}
	}
	return nil
}

// sharedLaneError is the refusal of checked, whose lane other holds too. It names other and its
// repository only to a viewer who sees other's project, or to no viewer at all (the boot seed,
// whose log only the operator reads); every other viewer reads hiddenLaneError, which names the
// lane alone, as GET /api/v1/settings/repo-projects hides a mapping of a project its caller cannot
// see.
func sharedLaneError(viewer *model.Viewer, lane string, checked, other repoProjectRow) error {
	if viewer != nil && !viewerSees(viewer, other.project) {
		return hiddenLaneError(lane)
	}
	return errorf(http.StatusConflict, codeRepositoryLaneShared, "%s and %s share the GitHub subject lane %s; %s",
		laneHolder(other, lane), laneHolder(checked, lane), lane, laneRule)
}

// hiddenLaneError is the refusal that names neither the project holding lane nor its repository.
func hiddenLaneError(lane string) error {
	return errorf(http.StatusConflict, codeRepositoryLaneShared, "the GitHub subject lane %s is held by a project you cannot see; %s",
		lane, laneRule)
}

// laneHolder names mapping as the holder of lane: its repository, and the name it was renamed to
// when that name is the one on the lane, then its project.
func laneHolder(mapping repoProjectRow, lane string) string {
	if mapping.renamedTo != nil && repoLanes(mapping.repo, nil)[0] != lane {
		return fmt.Sprintf("%s renamed to %s (%s)", mapping.repo, *mapping.renamedTo, mapping.project)
	}
	return fmt.Sprintf("%s (%s)", mapping.repo, mapping.project)
}

// viewerSees reports whether viewer sees project; no viewer is the server's own read.
func viewerSees(viewer *model.Viewer, project string) bool {
	return viewer == nil || viewer.All || slices.Contains(viewer.Sees, project)
}

// refuseHiddenMapping refuses a PUT of repo over mapping, the repository's mapping as its write's
// locks hold it (lockRepoProjectWrite), when mapping is of a project viewer cannot see: with the
// lane refusal's hidden form for repo's own lane, the bytes a PUT of a neighbour on that lane is
// answered, so the caller cannot tell the repository's own name from a neighbour's. It runs
// before refuseRepoProjectInUse, so REPO_PROJECT_IN_USE, which names the project and its issue
// count, answers only a caller who sees the project. The DELETE of such a mapping answers 204 and
// writes nothing, as for a repository no mapping holds (hiddenMapping).
func refuseHiddenMapping(viewer *model.Viewer, repo string, mapping *model.RepoProject) error {
	if !hiddenMapping(viewer, mapping) {
		return nil
	}
	return hiddenLaneError(repoLanes(repo, nil)[0])
}

// hiddenMapping reports whether mapping is of a project viewer cannot see; nil is no mapping.
func hiddenMapping(viewer *model.Viewer, mapping *model.RepoProject) bool {
	return mapping != nil && !viewerSees(viewer, mapping.Project)
}

// ensureMappedPrivate writes project's private-projects record through tx when project is
// private as tx reads it, for a mapping write whose target project is project: the write's last
// step before its commit, so the record names the repository before any other connection can
// read the mapping (K7). It adds and never removes, so a mapping that leaves a private project,
// and a renamed_to it changes, leave their names on the record until the writer's own pass
// rewrites it from the committed rows. A public target makes no call. The read follows the
// write's locks on project's row (lockRepoProjectWrite), so a concurrent switch of project to
// private is either read here or reads this write's mapping itself.
func ensureMappedPrivate(ctx context.Context, tx pgx.Tx, bus BusProjects, project string) error {
	var private bool
	if err := tx.QueryRow(ctx, `select visibility = 'private' from projects where key = $1`, project).Scan(&private); err != nil {
		return err
	}
	if !private {
		return nil
	}
	return ensurePrivate(ctx, tx, bus, project)
}
