import { expect, test } from "bun:test";
import { privateSlackSubject, privateSlackThreadSubject } from "./private-slack";
import { subjectProject } from "./subject-project";

test("a private project's Slack lane is its Legion token's, and reads as that project", () => {
  const channel = privateSlackSubject("PRIV", "T1", "C0PRIV00001", "mention");
  const thread = privateSlackThreadSubject(
    "PRIV",
    "T1",
    "C0PRIV00001",
    "1700000000.000100",
    "mention"
  );
  expect(channel).toBe("notifications.legion.priv.slack.T1.C0PRIV00001.mention");
  expect(thread).toBe(
    "notifications.legion.priv.slack.T1.C0PRIV00001.thread.1700000000_000100.mention"
  );
  expect(subjectProject(channel)).toEqual({ project: "PRIV", ok: true });
  expect(subjectProject(thread)).toEqual({ project: "PRIV", ok: true });
});
