import { readdirSync, readFileSync } from "node:fs";
import * as path from "node:path";
import { dispatchToolSpecs } from "@legion/contracts/dispatch-tools";

const REPO_ROOT = path.resolve(import.meta.dir, "../../..");
const COMMANDS = path.join(REPO_ROOT, "packages/envoy-client/src/dispatch-commands");
const SKILL_NAME = /skill:\/\/([a-z0-9](?:[a-z0-9._-]*[a-z0-9])?)/g;
const COMMENT_LINE = /^\s*(\/\/|\/?\*)/;

/**
 * Every skill that the Dispatch text of both plugins names as `skill://<name>`, sorted: the tool
 * specs of @legion/contracts/dispatch-tools, and each line of envoy-client's dispatch commands that
 * is no comment, which the `dispatch` command prints. Oh My Pi reads that text through
 * @sjawhar/pi-envoy and Claude Code through claude-envoy, so the guard of each plugin holds these
 * names to the skills that plugin ships.
 */
export function sharedDispatchSkillNames(): string[] {
  const text = [
    JSON.stringify(dispatchToolSpecs),
    ...readdirSync(COMMANDS)
      .filter((file) => file.endsWith(".ts"))
      .map((file) =>
        readFileSync(path.join(COMMANDS, file), "utf8")
          .split("\n")
          .filter((line) => !COMMENT_LINE.test(line))
          .join("\n")
      ),
  ].join("\n");
  return [...new Set([...text.matchAll(SKILL_NAME)].map(([, name]) => name ?? ""))].sort();
}
