import type { ArchitectureSource, CreatedProject } from "@legion/contracts";
import {
  type CommandContext,
  type DispatchToolResult,
  defined,
  optionalString,
  stringArg,
} from "./context";
import { refusalWithCode } from "./failures";
import { namedPerson } from "./render-item";

/** One source's answer to `dispatch architecture-sync`: its imported commit, or the recorded
 *  failure and what of that source's model stays up. A source re-saved on another branch has no
 *  recorded commit yet but keeps its earlier components until an import replaces them. */
function architectureSyncLines(project: string, source: ArchitectureSource): string {
  if (source.last_error === null) {
    return `Synced ${project} architecture from ${source.repo}@${source.branch}: commit ${source.last_commit ?? "unknown"} (${source.last_sync_at ?? "unknown time"}).`;
  }
  let kept: string;
  if (source.last_commit !== null) {
    kept = `The previous model of ${source.repo} stays up (commit ${source.last_commit}).`;
  } else if (source.components > 0) {
    kept = `The previous model of ${source.repo} stays up (${source.components} ${source.components === 1 ? "component" : "components"}).`;
  } else {
    kept = `No model has ever imported from ${source.repo}.`;
  }
  return [
    `Sync failed for ${project} (${source.repo}@${source.branch}): ${source.last_error}`,
    kept,
  ].join("\n");
}

/** `dispatch project`: open a project. */
export async function projectCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, linkBase } = context;
  let project: CreatedProject;
  try {
    project = await client.createProject({
      key: stringArg(args, "key"),
      name: stringArg(args, "name"),
      ...defined("default_assignee", optionalString(args, "default_assignee")),
      actor,
    });
  } catch (error) {
    // PROJECT_EXISTS and INVALID_PROJECT ask for different things, and the description names
    // both by code. The INVALID_PROJECT that names default_assignee says what the call must
    // carry, so the server's own words reach the agent unchanged.
    throw refusalWithCode(error);
  }
  const href = new URL(`/projects/${encodeURIComponent(project.key)}`, linkBase).toString();
  // Every project names a default assignee and the create answers it, so it is absent only from
  // a Dispatch older than that answer, and the output then names nobody rather than a person it
  // was not given.
  const projectDefault = namedPerson(project.default_assignee);
  return {
    // The default assignee is on the create's own line: the project's unowned work lands on
    // that person, and every question on an issue nobody holds reaches them, so the agent
    // reads who that is without a second command.
    text:
      `Created project ${project.key} (${project.name}) at ${project.created_at}: ${href}` +
      (projectDefault === undefined
        ? ""
        : `\nIts unowned work lands on ${projectDefault}: a question on an issue nobody holds reaches them.`),
    details: {
      project: project.key,
      name: project.name,
      created_at: project.created_at,
      ...defined("default_assignee", projectDefault),
    },
  };
}

/** `dispatch projects`: every project Dispatch holds, by key. */
export async function projectsCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { client } = context;
  const projects = (await client.listProjects()).map(({ key, name }) => ({ key, name }));
  return {
    text:
      projects.length === 0
        ? "No projects."
        : projects.map(({ key, name }) => `${key} ${name}`).join("\n"),
    details: { projects },
  };
}

/** `dispatch architecture-sync`: import one project's architecture model from its sources. */
export async function architectureSyncCommand(
  context: CommandContext
): Promise<DispatchToolResult> {
  const { args, client } = context;
  const project = stringArg(args, "project");
  const repo = optionalString(args, "repo");
  const sources =
    repo === undefined
      ? await client.syncArchitectureSources(project)
      : [await client.syncArchitectureSource(project, repo)];
  return {
    text: sources.map((source) => architectureSyncLines(project, source)).join("\n"),
    details: {
      project,
      sources: sources.map((source) => ({
        repo: source.repo,
        branch: source.branch,
        commit: source.last_commit,
        error: source.last_error,
      })),
    },
  };
}
