/**
 * The notice a resumed agent gets when its role launcher found its workspace recreated since its
 * session was last written: `LEGION_WORKSPACE_RECREATED=true`, which the launcher sets on every
 * generation it starts (packages/daemon/internal/launcher, `WorkspaceRecreatedVariable`).
 * Provisioning built that workspace from the issue's branch as pushed, or, when nothing was, from
 * the base that branch started from, so what the session remembers doing there may be gone.
 *
 * The text and the identity of one process's notice live here because both harnesses tell a
 * worker the same thing (INFRA-220 D29): the Oh My Pi entry saves it as a custom message ahead of
 * the next turn, and the Claude Code pane's Legion server writes it into the session over the
 * bridge's inbox once its claim has booted. The helpers that read an Oh My Pi transcript branch
 * stay with that host (`packages/pi-legion/src/workspace-recreated.ts`), so neither host reaches
 * into the other's history model.
 */
export const WORKSPACE_RECREATED_VARIABLE = "LEGION_WORKSPACE_RECREATED";

/**
 * What the notice says of a workspace made again, in the words of the task the daemon gives a role
 * its idle release woke (`WorkspaceAfterRelease` in packages/daemon/internal/workflow/wake.go): what
 * the idle release keeps (a commit on a working copy or a bookmark that no remote branch or tag
 * holds, or a working-copy change, keeps the pod), what a workspace made again holds, and the one
 * work the release can take, a commit no working copy and no bookmark reaches. The daemon keeps no
 * copy of a workspace, so neither text says that unpushed work was kept, nor that nothing unpushed
 * was lost. The launcher cannot tell an idle release from any other end of the pod, so the notice
 * says this beside what any other end costs.
 * `workspace-recreated.test.ts` holds this text to the daemon's data file
 * `packages/daemon/internal/workflow/workspace-after-release.txt`.
 */
export const WORKSPACE_AFTER_RELEASE =
  "The idle release does not take a pod whose workspace holds a commit on a working copy or a bookmark " +
  "that no remote branch or tag holds, or a working-copy change: such work stays in the pod. " +
  "When the pod's disk goes with it, the next pod's workspace is made again from what was pushed to this issue's branch " +
  "(or, if nothing was, the base that branch started from). " +
  "A commit that no working copy and no bookmark reaches, such as one left on a side head with jj new, is not kept.";

/**
 * One process's notice: its text, and the id every copy it sends carries (`details.id`), which
 * tells its copies from a notice an earlier recreation of the same session left in the history.
 */
export interface WorkspaceRecreatedNotice {
  readonly id: string;
  readonly content: string;
}

/** Whether the launcher of the process whose environment is `environment` recreated its
 * workspace. It decides this afresh for every launch. */
export function workspaceRecreated(
  environment: Readonly<Record<string, string | undefined>>
): boolean {
  return environment[WORKSPACE_RECREATED_VARIABLE] === "true";
}

/**
 * The notice for a Legion session on `issue` whose environment is `environment`, undefined when its
 * launcher said nothing was recreated.
 */
export function workspaceRecreatedNotice(
  environment: Readonly<Record<string, string | undefined>>,
  issue: string
): WorkspaceRecreatedNotice | undefined {
  if (!workspaceRecreated(environment)) return undefined;
  return {
    id: crypto.randomUUID(),
    content:
      `Your workspace was recreated since your last turn, from what was pushed to legion/${issue}. ` +
      `${WORKSPACE_AFTER_RELEASE} ` +
      "If anything else ended your pod (an eviction, a pod update, a reset, or an idle release made by a " +
      "Legion release before this one), work you had not pushed is gone. " +
      "What no commit held is gone in every case: the files your scratch " +
      "directory held, files git ignores, and anything you started that was running. " +
      "Before you continue, check the workspace against what you remember doing and re-read the " +
      `last handoff recorded on ${issue} with \`dispatch handoff-read --issue ${issue} --phase <phase>\` ` +
      "(plan, implement, test or review); Dispatch keeps it, whatever the workspace lost.",
  };
}
