import type { Deploy } from "@legion/contracts";
import { actorText } from "../actor-text";
import type { CommandContext, DispatchToolResult } from "./context";
import { defined, optionalBoolean, optionalString, stringArg } from "./context";

function deployText(deploy: Deploy): string {
  const link = deploy.url === "" ? "" : ` ${deploy.url}`;
  return `${deploy.program}: ${deploy.commit} since ${deploy.deployed_at} (source: ${deploy.source}, recorded by ${actorText(deploy.recorded_by)}).${link}`;
}

/** The CLI's shared dry-run path checks this command's schema and never calls this writer. */
export async function deployCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  const input = {
    repo: stringArg(args, "repo"),
    program: stringArg(args, "program"),
    commit: stringArg(args, "commit"),
    ...defined("deployed_at", optionalString(args, "deployed_at")),
    ...defined("url", optionalString(args, "url")),
  };
  const result = await client.recordDeploy(input);
  return {
    text:
      optionalBoolean(args, "json") === true
        ? JSON.stringify(result, null, 2)
        : deployText(result.deploy),
    details: { repo: input.repo, deploy: result.deploy },
  };
}

export async function deploysCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  const result = await client.deploys(stringArg(args, "repo"));
  const deploys = result.deploys ?? [];
  return {
    text:
      optionalBoolean(args, "json") === true
        ? JSON.stringify(result, null, 2)
        : deploys.length === 0
          ? `Dispatch holds no deploys for ${result.repo}.`
          : deploys.map(deployText).join("\n"),
    details: { repo: result.repo, deploys },
  };
}
