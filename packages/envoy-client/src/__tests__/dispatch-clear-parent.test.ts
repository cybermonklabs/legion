import { describe, expect, test } from "bun:test";
import { useIssueUpdateCli } from "./issue-update-cli";

// `dispatch issue-update --clear-parent` takes an issue out from under its parent: the command
// derives the flag from the nullable `parent` field of the spec, the executor sends the JSON null
// the server reads as "no parent", and the report says the parent was cleared. The fake Dispatch
// of `issue-update-cli.ts` records what the real CLI sent, as it does for `--clear-assignee` in
// `dispatch-asked-of.test.ts`; its DSP-1 sits under DSP-9 here.

const { run, calls, patchBody } = useIssueUpdateCli("dispatch-clear-parent", { parent: "DSP-9" });

describe("dispatch issue-update --clear-parent", () => {
  test("sends parent null and reports the parent cleared", async () => {
    const { code, stdout, stderr } = await run([
      "issue-update",
      "--issue",
      "DSP-1",
      "--clear-parent",
    ]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(calls()).toEqual(["GET /api/v1/issues/DSP-1", "PATCH /api/v1/issues/DSP-1"]);
    const body = patchBody();
    expect(Object.keys(body).sort()).toEqual(["actor", "parent"]);
    expect(body.parent).toBeNull();
    expect(stdout).toContain("DSP-1: parent cleared");
  });

  test("--parent still sends the key and reports it", async () => {
    const { code, stdout, stderr } = await run([
      "issue-update",
      "--issue",
      "DSP-1",
      "--parent",
      "DSP-2",
    ]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(patchBody().parent).toBe("DSP-2");
    expect(stdout).toContain("DSP-1: parent -> DSP-2");
  });

  test("is refused beside --parent, before any request", async () => {
    const { code, stdout } = await run([
      "issue-update",
      "--issue",
      "DSP-1",
      "--parent",
      "DSP-2",
      "--clear-parent",
    ]);

    expect(code).toBe(2);
    expect(stdout).toStartWith(
      "dispatch issue-update was not called: 1 problem\n- --parent and --clear-parent both set parent"
    );
    expect(calls()).toEqual([]);
  });

  test("--help says how to clear the parent", async () => {
    const { code, stdout, stderr } = await run(["issue-update", "--help"]);

    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(stdout).toContain("set or clear its parent (--clear-parent clears it)");
    expect(stdout).toContain("--clear-parent");
    expect(stdout).toContain("Sends parent as null.");
    expect(calls()).toEqual([]);
  });
});
