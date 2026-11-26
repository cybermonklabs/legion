import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import {
  WORKSPACE_AFTER_RELEASE,
  WORKSPACE_RECREATED_VARIABLE,
  workspaceRecreatedNotice,
} from "./workspace-recreated";

test("a launcher that recreated the workspace earns a notice naming the issue's branch", () => {
  const notice = workspaceRecreatedNotice({ [WORKSPACE_RECREATED_VARIABLE]: "true" }, "REPO-43");
  expect(notice?.content).toContain("from what was pushed to legion/REPO-43");
  expect(notice?.content).toContain("work you had not pushed is gone");
  expect(notice?.id).toMatch(/^[0-9a-f-]{36}$/);
});

/** The daemon's account of a workspace after an idle release: the data file its wake task embeds
 * (`WorkspaceAfterRelease`, packages/daemon/internal/workflow/wake.go), whose one line ends in a
 * newline the text does not carry. */
function daemonWorkspaceAfterRelease(): string {
  const repoRoot = path.resolve(import.meta.dir, "../../..");
  const file = path.join(repoRoot, "packages/daemon/internal/workflow/workspace-after-release.txt");
  return readFileSync(file, "utf8").replace(/\n$/u, "");
}

// A role woken after an idle release reads two texts about its workspace: the daemon's wake task
// and this notice. They say the same thing in the same words: what the release keeps (a commit on a
// working copy or a bookmark that no remote branch or tag holds, or a working-copy change, keeps
// the pod), what a workspace made again holds, and the one work the release can take. The daemon
// embeds the text from its data file; this test holds the notice to that file, so neither text
// moves alone.
test("the notice says of the workspace what the daemon's wake task says, in the same words", () => {
  const daemon = daemonWorkspaceAfterRelease();
  expect(WORKSPACE_AFTER_RELEASE).toBe(daemon);
  const notice = workspaceRecreatedNotice({ [WORKSPACE_RECREATED_VARIABLE]: "true" }, "REPO-43");
  expect(notice?.content).toContain(daemon);
});

// The daemon keeps no copy of a workspace, so the notice never says that work which was not pushed
// came back, and the release can take a commit no working copy and no bookmark reaches, so it never
// says that nothing unpushed was lost: it names that commit as not kept. Not even an older launcher
// that still sets LEGION_WORKSPACE_RESTORED earns another sentence.
test("the notice never says that work which was not pushed was kept, nor that none was lost", () => {
  for (const environment of [
    { [WORKSPACE_RECREATED_VARIABLE]: "true" },
    { [WORKSPACE_RECREATED_VARIABLE]: "true", LEGION_WORKSPACE_RESTORED: "true" },
  ]) {
    const content = workspaceRecreatedNotice(environment, "REPO-43")?.content ?? "";
    expect(content).not.toContain("put back");
    expect(content).not.toContain("saved");
    expect(content).not.toContain("as you left them");
    expect(content).not.toContain("holds nothing that was not pushed");
    expect(content).toContain(
      "A commit that no working copy and no bookmark reaches, such as one left on a side head with jj new, is not kept."
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
