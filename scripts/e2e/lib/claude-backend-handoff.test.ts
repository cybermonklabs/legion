import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { scriptCheckpoints } from "./script-functions";

// The Claude backend proofs run only against a live rig, so this file reads the text of their
// `claude-implementer-handoff` checkpoint and runs nothing. The daemon refuses a completion of
// implementing with no open pull request on `legion/<issue>`, unless GitHub cannot be read or a
// merged one is recorded (HANDOFF_NO_PULL_REQUEST, packages/daemon/internal/api/handoff.go,
// implementationPullRequest). So the checkpoint asks for the pull request before handoff_complete
// and checks that exactly one is open on the branch; the teardown's close_run_pull_requests
// closes it.
// The completion moves an issue with an open pull request out of implementing in the same step.
// The checkpoint waits for testing with wait_for_phase's own bound, never inside an until_true,
// whose poll sends the inner wait's FAIL line to /dev/null. It then reads the implementer's
// handoff, and assert_handoff_record checks the record's session and the commit's App.
const bounds: Record<string, number> = {
  "claude-backend-sandbox.sh": 1200,
  "claude-backend-tmux.sh": 900,
};
for (const [driver, bound] of Object.entries(bounds)) {
  const path = fileURLToPath(new URL(`../${driver}`, import.meta.url));
  const script = readFileSync(path, "utf8");
  const checkpoint = scriptCheckpoints(path)("claude-implementer-handoff");

  describe(driver, () => {
    test("the implementer is asked to open the pull request before handoff_complete", () => {
      expect(checkpoint).not.toContain("Do not open a pull request");
      expect(checkpoint).toContain(
        "open a pull request from legion/$issue to the repository's default branch, then call the legion tool's handoff_complete"
      );
    });

    test("the checkpoint waits for testing at its own bound, not inside a poll", () => {
      const lines = checkpoint.split("\n");
      const wait = lines.indexOf(`wait_for_phase "$issue" testing ${bound}`);
      expect(wait).toBeGreaterThan(0);
      expect(lines[wait - 1]?.endsWith("\\")).toBe(false);
      expect(checkpoint).not.toContain("handoff_recorded");
    });

    test("the checkpoint reads the implement handoff and checks its record after the move", () => {
      const lines = checkpoint.split("\n");
      const wait = lines.indexOf(`wait_for_phase "$issue" testing ${bound}`);
      expect(lines.slice(wait + 1, wait + 4)).toEqual([
        'commit=$(role_handoff "$issue" implementer testing)',
        'assert_round_handoff "$issue" 0',
        'assert_handoff_record "$issue" implementer implementing 0',
      ]);
    });

    test("the stall check's message reads the handoff in implementing or testing", () => {
      expect(script).toContain(
        '(handoff $(role_handoff "$issue" implementer implementing testing))'
      );
      expect(script).not.toContain('(handoff $(role_handoff "$issue" implementer implementing))');
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

test("the stall check sees a handoff that moved the issue to testing", () => {
  const helpers = readFileSync(
    fileURLToPath(new URL("./claude-transcript.sh", import.meta.url)),
    "utf8"
  );
  expect(helpers).toContain('role_handoff "$issue" implementer implementing testing');
});
