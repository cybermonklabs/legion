/**
 * An agent's name as its role's word, which an issue page's row draws (dispatch://INFRA-1196
 * ruling 6 in `dispatch://INFRA-1196/artifact/build-rulings-md`, R9's "an agent by its role as
 * its session's title gives it"): the title `AgentText.author` names it by (`AgentName.tsx`),
 * without the deployment's own word before it and without what it names the role for after it -
 * `Legion architect · INFRA-1196` reads `architect`, `Planner (issue layout)` reads `Planner`.
 * The Legion daemon keeps its whole title, which R9 names it by. A name with no such part is its
 * own role's word. The row's opened body draws the whole title (`NodeRow.tsx`).
 *
 * Pure, and in a module of its own, so the browser suite names an agent by the rule the page
 * does.
 */

/** Where a session's title stops naming its role: what it names it for, written after a middle
 *  dot, a bracket, a dash or a colon. */
const ROLE_END = /\s*(?:·|\(|\[|—|–|\s-\s|:)/u;

export function agentRoleWord(name: string): string {
  const title = name.trim();
  if (/^Legion daemon\b/u.test(title)) return title;
  const role =
    title
      .replace(/^Legion\s+/u, "")
      .split(ROLE_END)[0]
      ?.trim() ?? "";
  return role === "" ? title : role;
}
