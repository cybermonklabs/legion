import { describe, expect, test } from "bun:test";
import { parseCommand } from "../dispatch-command";

// `dispatch held`, `dispatch held-edit` and `dispatch held-post` (INFRA-3231's R5): the reviewer's
// findings frame writes `dispatch held-edit <id>` and `dispatch held-post <id> --reason '<reason>'`,
// so the held text's id is taken as the first word as well as from --held.

const noFiles = {
  readText: (path: string): string => {
    throw new Error(`unexpected read ${path}`);
  },
};

describe("the held-text commands", () => {
  test("take the held text's id as their first word, as the frame writes them", () => {
    expect(parseCommand(["held-post", "RVW-2", "--reason", "Right as written."], noFiles)).toEqual({
      kind: "call",
      tool: "dispatch_held_post",
      args: { held: "RVW-2", reason: "Right as written." },
      dryRun: false,
    });
    expect(
      parseCommand(
        ["held-edit", "01234567-0000-4000-8000-000000000001", "--body", "Changed."],
        noFiles
      )
    ).toEqual({
      kind: "call",
      tool: "dispatch_held_edit",
      args: { held: "01234567-0000-4000-8000-000000000001", body: "Changed." },
      dryRun: false,
    });
    expect(parseCommand(["held", "--held", "RVW-2"], noFiles)).toMatchObject({
      kind: "call",
      tool: "dispatch_held",
      args: { held: "RVW-2" },
    });
  });

  test("refuse a second id, and a bare word in any other place", () => {
    expect(parseCommand(["held", "RVW-2", "--held", "RVW-3"], noFiles)).toMatchObject({
      kind: "refused",
    });
    expect(parseCommand(["held-post", "--reason", "Why.", "RVW-2"], noFiles)).toMatchObject({
      kind: "refused",
    });
    // A command that takes no held text keeps refusing a bare word that is no issue key.
    expect(parseCommand(["whoami", "RVW"], noFiles)).toMatchObject({ kind: "refused" });
  });
});
