import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { scriptCheckpoints } from "./script-functions";

// Stage 4b's `done` checkpoint runs only against the production cluster, so this file holds its
// text to what the daemon does at the merge of tree 1. Children 3 and 4 were parked at
// children-first-acceptance, before the sign-off the daemon held for them, so nothing of tree 1 is
// in the workflow when its pull request merges, and the merge closes it at once: merging to done
// on pull_request_merged, with no close that waits and no pr-merged notice. The checkpoint counts
// the daemon's wait log and the deliveries of a pr-merged notice to tree 1's architect.
const path = fileURLToPath(new URL("../stage4b-sandbox-tree.sh", import.meta.url));

/** The `done` checkpoint's lines, from `begin "done"` to the next checkpoint's `begin`. */
const done = scriptCheckpoints(path)('"done"');

test("the done checkpoint waits for the merge to close tree 1 at once", () => {
  expect(done).toContain('on_tree "$tree1" wait_for_phase "$tree1" "done" 900');
  expect(done).toContain('phase_change_logged "$tree1" merging "done" pull_request_merged');
});

test("the done checkpoint fails on a close that waits and on a pr-merged notice for tree 1", () => {
  const waits = "workflow: a merged issue's close waits for the issues beneath it";
  expect(done).toContain(`log_lines "${waits}"`);
  expect(done).toContain('[ "$merge_waits" = 0 ]');
  expect(done).toContain(
    'notice_deliveries "$tree1" architect "$(notice_needle pr-merged "$tree1")"'
  );
  expect(done).toContain('[ "$merged_notices" = 0 ]');
  // The line the checkpoint counts is one the daemon writes, so a count of 0 is not a count of a
  // line nobody logs.
  const daemon = readFileSync(
    fileURLToPath(
      new URL("../../../packages/daemon/internal/workflow/pullrequest.go", import.meta.url)
    ),
    "utf8"
  );
  expect(daemon).toContain(`"${waits}"`);
});

test("the done checkpoint waits for no merged root left in merging and parks no child again", () => {
  expect(done).not.toContain("merge_recorded");
  expect(done).not.toContain("merged-waiting");
  expect(done).not.toContain('[ "$waiting_for" = "$child3, $child4" ]');
  expect(done).not.toContain("a merged root closes once its last child left the workflow");
  expect(done).not.toContain('set_status "$child" backlog');
});
