package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// projectOwnerActor is who a project's event names as giving it to its new owner when an operator
// runs `envoy-dispatch project-owner`.
var projectOwnerActor = model.Actor{Kind: "system", ID: "envoy-dispatch project-owner"}

// projectOwnerFallbackActor is who a project's event names as giving it to its owner when the boot
// gives a project with no owner to DISPATCH_PROJECT_OWNER_FALLBACK's full member.
var projectOwnerFallbackActor = model.Actor{Kind: "system", ID: "DISPATCH_PROJECT_OWNER_FALLBACK"}

// giveOwnerlessProject is the boot's giver (store.EnsureProjectOwners): project key goes to owner when
// it still has no owner, with the project.access_changed event and project_access_changes record
// `envoy-dispatch project-owner` writes, under projectOwnerFallbackActor (access.GiveOwnerless).
func giveOwnerlessProject(ctx context.Context, tx pgx.Tx, key, owner string) error {
	return access.GiveOwnerless(ctx, tx, events.NewBroker(), key, owner, projectOwnerFallbackActor)
}

// projectOwnerUsage is what `envoy-dispatch project-owner` prints for arguments it cannot take.
const projectOwnerUsage = "project-owner: give a project to another owner with `envoy-dispatch project-owner <KEY> <email>`, " +
	"where <email> is a full member who has signed in"

// giveProject runs `envoy-dispatch project-owner <KEY> <email>`: it gives project KEY to the full
// member email names, at any time and whoever owned it before, in one transaction that appends
// project.access_changed as projectOwnerActor when the owner changes (access.SetOwner); giving the
// project to its current owner writes nothing. It is how an operator gives an owner to a project
// the boot names as having none (store.EnsureProjectOwners), beside
// DISPATCH_PROJECT_OWNER_FALLBACK. It exits 0 once the change commits or when there is nothing to
// change, 2 for wrong arguments, a key no project holds, or an email that is not a full member's,
// writing nothing, and 1 when the database cannot be opened or written.
func giveProject(ctx context.Context, args []string, databaseURL string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, projectOwnerUsage)
		return 2
	}
	key, owner := args[0], args[1]
	database, ok := openMigrated(ctx, "project-owner", databaseURL, stderr)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	ctx = store.WithTransactionTracking(ctx)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "project-owner: begin: %v\n", err)
		return 1
	}
	defer tx.Rollback(ctx)
	given, event, err := access.SetOwner(ctx, tx, events.NewBroker(), key, owner, projectOwnerActor)
	if errors.Is(err, access.ErrProjectNotFound) || errors.Is(err, access.ErrOwnerNotAllowed) {
		fmt.Fprintf(stderr, "project-owner: %v\n", err)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "project-owner: %v\n", err)
		return 1
	}
	if err := tx.Commit(ctx); err != nil {
		fmt.Fprintf(stderr, "project-owner: commit: %v\n", err)
		return 1
	}
	if event.ID == 0 {
		fmt.Fprintf(stdout, "project-owner: project %s is owned by %s already; nothing written\n", given.Project, given.Owner)
		return 0
	}
	fmt.Fprintf(stdout, "project-owner: project %s is owned by %s (event %d)\n", given.Project, given.Owner, event.ID)
	return 0
}
