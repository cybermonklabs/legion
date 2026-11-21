package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// projectVisibilityActor is who a project's event names as making it public when an operator runs
// `envoy-dispatch project-visibility`.
var projectVisibilityActor = model.Actor{Kind: "system", ID: "envoy-dispatch project-visibility"}

// projectVisibilityUsage is what `envoy-dispatch project-visibility` prints for arguments it cannot
// take.
const projectVisibilityUsage = "project-visibility: make a project public with `envoy-dispatch project-visibility <KEY> public`; " +
	"it makes a project public only, the repair of a private project neither DISPATCH_PRIVATE_PROJECTS nor DISPATCH_PRIVATE_BEFORE_FULL_SWITCH names any longer"

// makeProjectPublic runs `envoy-dispatch project-visibility <KEY> public`: it makes project KEY
// public without the server, in one transaction that appends project.access_changed as
// projectVisibilityActor (access.SetVisibility), as the owner's own switch from the Access tab
// does. It is the repair of a private project neither DISPATCH_PRIVATE_PROJECTS nor
// DISPATCH_PRIVATE_BEFORE_FULL_SWITCH names any longer, beside putting the key back on a list. It
// makes no bus call. The hub's private-projects record has no writer in this build: main leaves
// routes.AppContextOptions.BusProjects nil, so every full switch to private answers 409
// PRIVATE_PROJECTS_UNAVAILABLE, a switch before the full one writes no record, so no key is written
// there, and none is deleted when a project is made public. It exits 0 once the change commits, 2 for
// wrong arguments, a value other than public or a key no project holds, writing nothing, and 1
// when the database cannot be opened or written.
func makeProjectPublic(ctx context.Context, args []string, databaseURL string, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[1] != model.ProjectPublic {
		fmt.Fprintln(stderr, projectVisibilityUsage)
		return 2
	}
	key := args[0]
	database, ok := openMigrated(ctx, "project-visibility", databaseURL, stderr)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	ctx = store.WithTransactionTracking(ctx)
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "project-visibility: begin: %v\n", err)
		return 1
	}
	defer tx.Rollback(ctx)
	made, event, err := access.SetVisibility(ctx, tx, events.NewBroker(), key, model.ProjectPublic, projectVisibilityActor)
	if errors.Is(err, access.ErrProjectNotFound) {
		fmt.Fprintf(stderr, "project-visibility: %v\n", err)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "project-visibility: %v\n", err)
		return 1
	}
	if err := tx.Commit(ctx); err != nil {
		fmt.Fprintf(stderr, "project-visibility: commit: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "project-visibility: project %s is %s (event %d)\n", made.Project, made.Visibility, event.ID)
	return 0
}
