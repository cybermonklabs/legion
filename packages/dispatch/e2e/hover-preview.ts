/** The one reference the hover-preview rows read: a comment on one issue naming another.
 *
 *  Two spec files share it: `hover-preview.stub.e2e.ts`, whose rows read only what the page
 *  draws, and `hover-preview.e2e.ts`, which holds the phone row the `iphone` project runs.
 */
import { createComment, createIssue, createProject, patchIssue, type RowKey } from "./api";

const agent = {
  actor: { id: "e2e-hover-agent", kind: "session" as const },
  as: "agent" as const,
};

/** `project` is the key of the project both issues are made in: `hover-preview.e2e.ts` passes
 *  its row's own (`rowKey`, e2e/test.ts), and the stubbed rows replay a recording made under
 *  `CORE`. */
export async function seedReference(project: RowKey): Promise<{ source: string; target: string }> {
  await createProject({ key: project, name: "Core" });
  const target = await createIssue({
    project,
    spec: "The target's spec explains what hovering should reveal about it.",
    title: "Hover target",
  });
  await patchIssue(target.key, { labels: ["ui"], priority: 1, status: "todo" });
  const source = await createIssue({ project, title: "Hover source" });
  await createComment(source.key, { body: `See dispatch://${target.key} for context.` }, agent);
  return { source: source.key, target: target.key };
}
