import { expect, test } from "bun:test";
import type { PiApi } from "@legion/pi-shared/pi-types";
import {
  restartMessage,
  type StallCall,
  type StallOverdue,
  TURN_ENDED_NOTICE,
} from "@legion/pi-shared/stall-watchdog";
import {
  WORKSPACE_RECREATED_NOTICE,
  WORKSPACE_RECREATED_VARIABLE,
} from "@legion/pi-shared/workspace-recreated";
import { noticeProblems } from "@legion/test-fixtures/notice-fields";
import { createStallAborter } from "./stall-abort";
import { workspaceRecreatedMessage, workspaceRecreatedNotice } from "./workspace-recreated";

/** The text of the workspace notice this host saves for a session on `issue`. */
function workspaceShown(issue: string): string {
  const notice = workspaceRecreatedNotice({ [WORKSPACE_RECREATED_VARIABLE]: "true" }, issue);
  if (notice === undefined) throw new Error("a recreated workspace earned no notice");
  return workspaceRecreatedMessage(notice).content;
}

/** The text of the restart message this host steers into the session for `call`. */
async function restartShown(call: StallCall, overdue: StallOverdue): Promise<string> {
  const sent: string[] = [];
  const sendMessage: PiApi["sendMessage"] = (message) => {
    if ("content" in message) sent.push(String(message.content));
  };
  await createStallAborter({ context: () => undefined, sendMessage }).sendMessage(
    restartMessage(call, overdue)
  );
  const [text] = sent;
  if (text === undefined) throw new Error("the aborter steered nothing");
  return text;
}

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

// Each notice this plugin shows its session, as it shows it, each variant of it rendered twice
// from different values in every field: a sentence the same in every notice of its kind is what
// two renderings would share.
test("every plugin notice states its fields", async () => {
  const renderings = [
    {
      declared: WORKSPACE_RECREATED_NOTICE,
      variants: [[workspaceShown("REPO-43"), workspaceShown("CORE-7")]],
    },
    {
      declared: TURN_ENDED_NOTICE,
      variants: [
        [
          await restartShown(call("bash", "toolu_stuck"), {
            rule: { kind: "bound", boundMs: 1_200_000 },
            openMs: 1_260_000,
            pastMs: 60_000,
          }),
          await restartShown(call("read", "toolu_02Lw"), {
            rule: { kind: "bound", boundMs: 600_000 },
            openMs: 645_000,
            pastMs: 45_000,
          }),
        ],
        [
          await restartShown(call("eval", "toolu_01Kq"), {
            rule: { kind: "deadline", deadlineMs: 120_000, graceMs: 300_000 },
            openMs: 431_000,
            pastMs: 11_000,
          }),
          await restartShown(call("python", "toolu_03Zr"), {
            rule: { kind: "deadline", deadlineMs: 30_000, graceMs: 90_000 },
            openMs: 127_000,
            pastMs: 7_000,
          }),
        ],
      ],
    },
  ] as const;
  for (const { declared, variants } of renderings) {
    expect(noticeProblems(declared, variants)).toEqual([]);
  }
});
