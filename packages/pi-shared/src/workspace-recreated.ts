import { noticeKind, noticeText } from "@legion/envoy-client/notice-text";

/**
 * The notice a resumed agent gets when its role launcher found its workspace recreated since its
 * session was last written: `LEGION_WORKSPACE_RECREATED=true`, which the launcher sets on every
 * generation it starts (packages/daemon/internal/launcher, `WorkspaceRecreatedVariable`).
 * Provisioning built that workspace from the issue's branch as pushed, the repository's default
 * branch when nothing was, so what the session remembers doing there may be gone.
 *
 * The notice and the identity of one process's notice live here because both harnesses tell a
 * worker the same thing (INFRA-220 D29): the Oh My Pi entry saves it as a custom message ahead of
 * the next turn, and the Claude Code pane's Legion server writes it into the session over the
 * bridge's inbox once its claim has booted. The helpers that read an Oh My Pi transcript branch
 * stay with that host (`packages/pi-legion/src/workspace-recreated.ts`), so neither host reaches
 * into the other's history model.
 */
export const WORKSPACE_RECREATED_VARIABLE = "LEGION_WORKSPACE_RECREATED";

/**
 * The recreated-workspace notice: the issue and the issue's branch (`legion/<KEY>`), which
 * provisioning built the workspace from where anything was pushed to it. The daemon keeps no copy of
 * a workspace, and the launcher cannot tell an idle release from any other end of the pod, so the
 * notice states nothing of the work the session had not pushed. What that is, and what the agent
 * does on the notice, is its role prompt's passage on `notice: workspace recreated`
 * (packages/daemon/internal/prompts/go/worker-common.md and architect-common.md): what the idle
 * release keeps, in the words of the same prompt's passage on the daemon's wake task after an idle
 * release, which `workspace-recreated.test.ts` holds each such passage to, and what any other end
 * of the pod costs.
 */
export const WORKSPACE_RECREATED_NOTICE = noticeKind("legion", "workspace recreated", [
  "issue",
  "branch",
]);

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
    content: noticeText(WORKSPACE_RECREATED_NOTICE, { issue, branch: `legion/${issue}` }),
  };
}
