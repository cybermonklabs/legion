import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

// The Claude backend proofs run only against a live rig, so this file holds the text of their
// `claude-implementer-handoff` checkpoint to what the daemon does: it refuses a completion of
// implementing while no pull request is open on `legion/<issue>` (HANDOFF_NO_PULL_REQUEST,
// packages/daemon/internal/api/handoff.go, implementationPullRequest). An implementer told to
// open none could complete only where GitHub's read failed, so the checkpoint asks for the pull
// request before handoff_complete and checks that exactly one is open on the branch; the
// teardown's close_run_pull_requests closes it.
for (const driver of ["claude-backend-sandbox.sh", "claude-backend-tmux.sh"]) {
  const script = readFileSync(fileURLToPath(new URL(`../${driver}`, import.meta.url)), "utf8");

  /** The checkpoint's lines, from its `begin` to the next checkpoint's `begin`. */
  const checkpoint = (() => {
    const lines = script.split("\n");
    const start = lines.indexOf("begin claude-implementer-handoff");
    const end = lines.findIndex((text, index) => index > start && text.startsWith("begin "));
    if (start < 0 || end < 0) {
      throw new Error(`the checkpoint claude-implementer-handoff is not in ${driver}`);
    }
    return lines.slice(start, end).join("\n");
  })();

  describe(driver, () => {
    test("the implementer is asked to open the pull request before handoff_complete", () => {
      expect(checkpoint).not.toContain("Do not open a pull request");
      expect(checkpoint).toContain(
        "open a pull request from legion/$issue to the repository's default branch, then call the legion tool's handoff_complete"
      );
    });

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
