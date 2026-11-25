import { useQuery } from "@tanstack/react-query";

import { projectsQuery } from "../../api/queries";
import type { Project } from "../../api/types";
import { issueProject } from "../../lib/issue-key";

/**
 * Which bare issue keys a reader of a text asks Dispatch about. A word of prose shaped like an
 * issue key - `UTF-16`, `SHA-256` - names an issue only where its prefix is a project. Where the
 * page already holds its project list (the sidebar reads it, `GET /api/v1/projects`, every
 * project there is), a key of no project is asked nothing and stays the words written; where it
 * holds none yet, every key is asked, since only its read can say. The one rule for both readers
 * of a text: a rendered body's (`useTextReferences`) and its plain-text projection's
 * (`plain-text.tsx`, `markdown-engine.ts`'s `candidateTarget`).
 */

/** The keys of a project list. */
function projectKeys(projects: readonly Project[]): ReadonlySet<string> {
  return new Set(projects.map((project) => project.key));
}

/** The keys of the projects the page holds, or undefined while it holds no list: observed, and
 *  never read for this. */
export function useProjectKeys(): ReadonlySet<string> | undefined {
  return useQuery({ ...projectsQuery(), enabled: false, select: projectKeys }).data;
}

/** Whether a bare issue key may name an issue, and so is worth a read: its prefix is a project
 *  `projects` holds, or the page holds no project list. */
export function keyMayNameIssue(key: string, projects: ReadonlySet<string> | undefined): boolean {
  return projects === undefined || projects.has(issueProject(key));
}
