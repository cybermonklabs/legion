import { expect, test } from "bun:test";
import { noticeProblems } from "@legion/test-fixtures/notice-fields";
import { restartMessage, type StallCall, TURN_ENDED_NOTICE } from "./stall-watchdog";
import {
  WORKSPACE_RECREATED_NOTICE,
  WORKSPACE_RECREATED_VARIABLE,
  workspaceRecreatedNotice,
} from "./workspace-recreated";

/** An open call of `toolName` with the id `toolCallId`, as the watch records it. */
function call(toolName: string, toolCallId: string): StallCall {
  return {
    toolCallId,
    toolName,
    input: {},
    startedAt: 0,
    deadlineMs: undefined,
    step: 1,
    steppedAt: undefined,
    told: undefined,
  };
}

// Each notice this package builds for Legion's plugins, each variant of it rendered twice from
// different values in every field: a sentence the same in every notice of its kind is what two
// renderings would share.
test("every plugin notice states its fields", () => {
  const workspace = (issue: string) =>
    workspaceRecreatedNotice({ [WORKSPACE_RECREATED_VARIABLE]: "true" }, issue)?.content ?? "";
  const renderings = [
    {
      declared: WORKSPACE_RECREATED_NOTICE,
      variants: [[workspace("REPO-43"), workspace("CORE-7")]],
    },
    {
      declared: TURN_ENDED_NOTICE,
      variants: [
        [
          restartMessage(call("eval", "toolu_01Aa"), {
            rule: { kind: "deadline", deadlineMs: 300_000, graceMs: 60_000 },
            openMs: 412_400,
            pastMs: 52_400,
          }),
          restartMessage(call("python", "toolu_03Cc"), {
            rule: { kind: "deadline", deadlineMs: 45_000, graceMs: 15_000 },
            openMs: 71_800,
            pastMs: 11_800,
          }),
        ],
        [
          restartMessage(call("bash", "toolu_02Bb"), {
            rule: { kind: "bound", boundMs: 900_000 },
            openMs: 1_000_200,
            pastMs: 100_200,
          }),
          restartMessage(call("fetch", "toolu_04Dd"), {
            rule: { kind: "bound", boundMs: 1_800_000 },
            openMs: 1_830_600,
            pastMs: 30_600,
          }),
        ],
      ],
    },
  ] as const;
  for (const { declared, variants } of renderings) {
    expect(noticeProblems(declared, variants)).toEqual([]);
  }
});
