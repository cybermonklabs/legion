import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { scriptCheckpoints } from "./script-functions";

// The Claude backend proofs run only against a live rig, so this file holds the text of their
// `claude-implementer-handoff` checkpoint to what the daemon does: it refuses a completion of
// implementing while no pull request is open on `legion/<issue>` (HANDOFF_NO_PULL_REQUEST,
// packages/daemon/internal/api/handoff.go, implementationPullRequest). An implementer told to
// open none could complete only where GitHub's read failed, so the checkpoint asks for the pull
// request before handoff_complete and checks that exactly one is open on the branch; the
// teardown's close_run_pull_requests closes it.
for (const driver of ["claude-backend-sandbox.sh", "claude-backend-tmux.sh"]) {
  const path = fileURLToPath(new URL(`../${driver}`, import.meta.url));
  const script = readFileSync(path, "utf8");
  const checkpoint = scriptCheckpoints(path)("claude-implementer-handoff");

  describe(driver, () => {
    test("the checkpoint checks that exactly one pull request is open on the issue branch", () => {
      expect(checkpoint).toContain(
        'open_prs=$(gh -R "$repo" pr list --state open --head "legion/$issue" --json number --jq length)'
      );
      expect(checkpoint).toContain('[ "$open_prs" = 1 ]');
    });

    test("the header no longer says the implementer opens no pull request", () => {
      expect(script).not.toContain("opens no pull request");
    });
  });
}
