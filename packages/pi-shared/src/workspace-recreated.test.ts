import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { noticeFields } from "@legion/test-fixtures/notice-fields";
import { WORKSPACE_RECREATED_VARIABLE, workspaceRecreatedNotice } from "./workspace-recreated";

const repoRoot = path.resolve(import.meta.dir, "../../..");

test("a launcher that recreated the workspace earns a notice naming the issue's branch", () => {
  const notice = workspaceRecreatedNotice({ [WORKSPACE_RECREATED_VARIABLE]: "true" }, "REPO-43");
  expect(noticeFields(notice?.content ?? "")).toEqual({
    notice: "workspace recreated",
    issue: "REPO-43",
    branch: "legion/REPO-43",
  });
  expect(notice?.id).toMatch(/^[0-9a-f-]{36}$/);
});

/** The daemon's account of a workspace after an idle release, as main's wake task and this notice
 * said it at 5d64e1b7 (the one line of that commit's
 * packages/daemon/internal/workflow/workspace-after-release.txt, without its newline). The daemon's
 * step table (TestEveryStepTakenOutOfATextIsInItsReadersPrompt) holds each reader's passage on the
 * daemon's wake task to the same words. */
const WORKSPACE_AFTER_RELEASE =
  "The idle release does not take a pod whose workspace holds a commit on a working copy or a bookmark that no remote branch or tag holds, or a working-copy change: such work stays in the pod. When the pod's disk goes with it, the next pod's workspace is made again from what was pushed to this issue's branch (the repository's default branch if nothing was). A commit that no working copy and no bookmark reaches, such as one left on a side head with jj new, is not kept.";

/** Each line of the role prompt `file` (packages/daemon/internal/prompts/go) that holds the
 * passage on this notice. */
function noticePassages(file: string): string[] {
  const body = readFileSync(
    path.join(repoRoot, "packages/daemon/internal/prompts/go", file),
    "utf8"
  );
  return body.split("\n").filter((line) => line.includes("`notice: workspace recreated`"));
}

/** The prompts of every role that reads this notice: each role of an issue pod, the architect
 * among them. */
const READERS_PROMPTS = ["worker-common.md", "architect-common.md"] as const;

// The words that say unpushed work was kept, which the notice and its passages never say: the
// daemon keeps no copy of a workspace.
const KEPT = [
  "put back",
  "saved",
  "restored",
  "as you left them",
  "holds nothing that was not pushed",
] as const;

// A role woken after an idle release reads two accounts of its workspace: its prompt's passage on
// the daemon's wake task and its prompt's passage on this notice. They say the same thing in the
// same words: what the release keeps (a commit on a working copy or a bookmark that no remote
// branch or tag holds, or a working-copy change, keeps the pod), what a workspace made again holds,
// and the one work the release can take. The notice states its fields alone, so the words are the
// passage's: this test holds the passage of each reader's prompt to main's words, which the
// daemon's step table holds the passage on its wake task to, so neither passage moves alone.
test("the prompts' passages on the notice say of the workspace what main's wake task said, in the same words", () => {
  for (const file of READERS_PROMPTS) {
    const passages = noticePassages(file);
    expect(passages, file).toHaveLength(1);
    expect(passages[0], file).toContain(WORKSPACE_AFTER_RELEASE);
  }
});

// The daemon keeps no copy of a workspace, so neither the notice nor its passage says that work
// which was not pushed came back, and the release can take a commit no working copy and no
// bookmark reaches, so the passage never says that nothing unpushed was lost: it names that commit
// as not kept. Not even an older launcher that still sets LEGION_WORKSPACE_RESTORED earns another
// field.
test("the notice never says that work which was not pushed was kept, nor that none was lost", () => {
  for (const environment of [
    { [WORKSPACE_RECREATED_VARIABLE]: "true" },
    { [WORKSPACE_RECREATED_VARIABLE]: "true", LEGION_WORKSPACE_RESTORED: "true" },
  ]) {
    const content = workspaceRecreatedNotice(environment, "REPO-43")?.content ?? "";
    expect(Object.keys(noticeFields(content))).toEqual(["notice", "issue", "branch"]);
    for (const kept of KEPT) expect(content).not.toContain(kept);
  }
  for (const file of READERS_PROMPTS) {
    const [passage = ""] = noticePassages(file);
    for (const kept of KEPT) expect(passage, file).not.toContain(kept);
    expect(passage, file).toContain(
      "A commit that no working copy and no bookmark reaches, such as one left on a side head with jj new, is not kept."
    );
    expect(passage, file).toContain(
      "If anything else ended your pod (an eviction, a pod update, a reset, or an idle release made by a Legion release before this one), work you had not pushed is gone."
    );
  }
});

// The id is what a host tells this process's copies of the notice by, so two notices never share
// one.
test("each notice carries an id of its own", () => {
  const environment = { [WORKSPACE_RECREATED_VARIABLE]: "true" };
  const first = workspaceRecreatedNotice(environment, "REPO-43");
  const second = workspaceRecreatedNotice(environment, "REPO-43");
  expect(first?.id).not.toBe(second?.id);
});

test("a launcher that recreated nothing says nothing, and only `true` is recreated", () => {
  expect(workspaceRecreatedNotice({}, "REPO-43")).toBeUndefined();
  for (const value of ["", "false", "TRUE", "1"]) {
    expect(
      workspaceRecreatedNotice({ [WORKSPACE_RECREATED_VARIABLE]: value }, "REPO-43")
    ).toBeUndefined();
  }
});
