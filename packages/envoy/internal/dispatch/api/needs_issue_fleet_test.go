package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A plan task's needs names an issue under its issue from schema version 9 (R11 of the issue-tree
// spec), an item kind a build of version 8 reads as a value outside the schema rather than as newer
// content. So a write introduces one only while every process serving the database reads version 9
// - the fleet's creatable number, which a page is admitted writable under - and is refused, in the
// words of a write of an element its build may not create yet, while a process of version 8 lives.
// A write that keeps a needs the document already holds, or adds a needs at a plan task, is taken
// all the while, and once no process of version 8 lives the issue needs is taken.
func TestAnIssueUnderNeedsIsWrittenOnlyWhileTheFleetCreatesIt(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	parent := createSpecIssue(t, handler, "CORE", "The parent's spec.\n")
	first := createChildIssue(t, handler, "CORE", parent.Key, "The first child's spec.\n")
	second := createChildIssue(t, handler, "CORE", parent.Key, "The second child's spec.\n")
	checks := "dispatch://" + parent.Key + "/artifact/checks-md"
	task := func(id, needs string) string {
		return ":::plan_task{#" + id + " needs=\"" + needs + "\"}\nA task.\n:::\n\n"
	}
	upload := func(content string) int {
		t.Helper()
		return uploadChecks(t, handler, parent.Key, ":::plan_task{#t1}\nThe first task.\n:::\n\n"+content).Code
	}

	// Every process reads version 9: the issue needs is taken.
	withFirst := task("t2", "dispatch://"+first.Key)
	if code := upload(withFirst); code != http.StatusCreated {
		t.Fatalf("an issue needs while every process reads version 9: status %d, want 201", code)
	}

	// A process of version 8 joins the fleet, with the creatable number a build of version 8
	// registers, 7 (the highest since among the elements it may create). It registers this build's
	// owner lock protocol, as the release before does, so the owner locks stay in this build's mode.
	ctx := context.Background()
	var instance string
	if err := database.Pool.QueryRow(ctx, `
		insert into server_processes (instance, signing_keys, schema_version, schema_creatable, heartbeat_at, owner_lock_protocol)
		values (gen_random_uuid(), '{fingerprint}', 8, 7, now(), $1) returning instance::text`, store.OwnerLockProtocol).Scan(&instance); err != nil {
		t.Fatalf("add a live server process of schema version 8: %v", err)
	}
	requireNoLegacyOwnerLockMark(t, database)
	if code := upload(withFirst + task("t3", checks+"#b-t1")); code != http.StatusCreated {
		t.Fatalf("a write keeping the issue needs and adding a plan task's needs while a process of version 8 lives: status %d, want 201", code)
	}
	withSecond := withFirst + task("t3", checks+"#b-t1") + task("t4", "dispatch://"+second.Key)
	expectRefusal(t, "an issue needs while a process of version 8 lives", uploadChecks(t, handler, parent.Key, ":::plan_task{#t1}\nThe first task.\n:::\n\n"+withSecond),
		http.StatusBadRequest, "INVALID_MARKDOWN", `"needs"`, "dispatch://"+second.Key,
		`this Dispatch build reads item "issue" of attribute "needs" of typed block "plan_task" but cannot write it yet`)

	// The process of version 8 is gone: the issue needs is taken.
	if _, err := database.Pool.Exec(ctx, `delete from server_processes where instance = $1::uuid`, instance); err != nil {
		t.Fatalf("remove the process of version 8: %v", err)
	}
	if code := upload(withSecond); code != http.StatusCreated {
		t.Fatalf("an issue needs once every process reads version 9: status %d, want 201", code)
	}
}
