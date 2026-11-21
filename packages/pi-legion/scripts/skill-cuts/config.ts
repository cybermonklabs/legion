/**
 * The configuration of the skill-cuts runner: one JSON file, `skill-cuts.json`, passed to every
 * command as `--config <file>`, and `loadConfig` is the one loader every command uses. This
 * repository is public, so every private value the runner needs (the GitHub owners it lists, the
 * session stores it counts, the program that reaches them, the model gateway's address, the paths
 * it leaves out and the repositories its copies map to) comes from this file at run time and never
 * from a committed file. The real file is kept outside the repository.
 *
 * Keys, every one required but the last, no other allowed:
 *   owners          GitHub owners whose repositories the copy lister reads
 *   stores          the session stores the count reads: `{name, context}`, `name` the database
 *                   name, `context` `devbox` or `legion:<PROJECT>` (whose sessions the store holds)
 *   psqlCommand     absolute path of a program that runs the store's read command for the database
 *                   its first argument names, with the query on its standard input
 *   gatewayBaseUrl  the model gateway's base URL
 *   servedModels    the models the gateway serves, each `provider/model` (`anthropic/claude-x`);
 *                   the read count's models tables mark a session's model served when its id
 *                   after the last `/` is one of these ids after theirs
 *   excludePaths    `{repo, prefix}`: the paths of a repository whose skills are left out
 *   targets         repositories (`owner/name`) the copy-prefix table may map a copy to
 *   cuts            `{id, repo, pr}`: each skill cut, the pull request that made it
 *   windowStart     the count's window, start inclusive, as an ISO instant (`…Z`)
 *   windowEnd       the window's end, exclusive, after windowStart
 *   devboxClaudeConfigDir  (optional) the absolute path a dev box's CLAUDE_CONFIG_DIR names, whose
 *                   CLAUDE.md Oh My Pi reads as its user-level context file: a devbox capture
 *                   shows that file at `<dir>/CLAUDE.md` (capture.ts, "The harness strings"),
 *                   and a neutral path without it. It names a person's home on the dev box, a
 *                   private value that only this file holds.
 *
 * A file that is not JSON, or that misses a key, holds a key not above or holds a value of the
 * wrong shape, is refused with one line per fault, each naming the key.
 */
import { readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

/** A repository as `owner/name`. */
const Repository = z.string().regex(/^[A-Za-z0-9][A-Za-z0-9-]*\/[A-Za-z0-9._-]+$/, {
  error: "a repository as owner/name",
});
/** An instant in UTC, as the window's ends are written: `2026-10-23T00:00:00Z`. */
const Instant = z.iso.datetime({ error: "an ISO instant ending in Z" });

/** A session store: its database name, which a query names only as a value, so it is held to the
 *  letters a database identifier needs, and whose sessions it holds. */
export const Store = z.strictObject({
  name: z.string().regex(/^[a-z_][a-z0-9_]*$/, {
    error: "a database name of small letters, digits and _",
  }),
  context: z.string().regex(/^(devbox|legion:[A-Z][A-Z0-9]*)$/, {
    error: "devbox or legion:<PROJECT>",
  }),
});
export type Store = z.infer<typeof Store>;

export const SkillCutsConfig = z
  .strictObject({
    owners: z
      .array(z.string().regex(/^[A-Za-z0-9][A-Za-z0-9-]*$/, { error: "a GitHub owner" }))
      .min(1),
    stores: z.array(Store).min(1),
    psqlCommand: z
      .string()
      .refine((value) => path.isAbsolute(value), { error: "an absolute path" }),
    gatewayBaseUrl: z.url({ protocol: /^https?$/, error: "an http or https URL" }),
    servedModels: z.array(
      z.string().regex(/^[^\s/]+(\/[^\s/]+)+$/, { error: "a model as provider/model" })
    ),
    excludePaths: z.array(z.strictObject({ repo: Repository, prefix: z.string().min(1) })),
    targets: z.array(Repository),
    cuts: z.array(
      z.strictObject({ id: z.string().min(1), repo: Repository, pr: z.number().int().positive() })
    ),
    windowStart: Instant,
    windowEnd: Instant,
    devboxClaudeConfigDir: z
      .string()
      .refine((value) => path.isAbsolute(value), { error: "an absolute path" })
      .optional(),
  })
  .refine((config) => Date.parse(config.windowStart) < Date.parse(config.windowEnd), {
    error: "windowEnd must be after windowStart",
    path: ["windowEnd"],
  });
export type SkillCutsConfig = z.infer<typeof SkillCutsConfig>;

/** Where an issue is, as a reader finds the key: `stores[1].name`, or `the top level`. */
function where(issuePath: readonly PropertyKey[]): string {
  if (issuePath.length === 0) return "the top level";
  return issuePath
    .map((key, index) =>
      typeof key === "number" ? `[${key}]` : `${index === 0 ? "" : "."}${String(key)}`
    )
    .join("");
}

/** One line per fault of a value that is not a configuration, each naming its key. */
function faults(error: z.ZodError): string[] {
  return error.issues.map((issue) => {
    if (issue.code === "unrecognized_keys") {
      const keys = issue.keys.map((key) => `"${key}"`).join(", ");
      return `${where(issue.path)}: unknown key ${keys}`;
    }
    if (issue.code === "invalid_type" && issue.input === undefined && issue.path.length > 0) {
      return `${where(issue.path)}: missing`;
    }
    return `${where(issue.path)}: ${issue.message}`;
  });
}

/** The configuration a parsed JSON value holds; throws, naming each fault's key, when it is
 *  none. */
export function parseConfig(value: unknown, source = "the configuration"): SkillCutsConfig {
  const parsed = SkillCutsConfig.safeParse(value, { reportInput: true });
  if (!parsed.success) {
    throw new Error(`${source} is refused:\n  ${faults(parsed.error).join("\n  ")}`);
  }
  return parsed.data;
}

/** The configuration in the file `file`; throws when it cannot be read, is not JSON, or is not a
 *  configuration. */
export function loadConfig(file: string): SkillCutsConfig {
  let text: string;
  try {
    text = readFileSync(file, "utf8");
  } catch (error) {
    throw new Error(`cannot read the configuration ${file}: ${(error as Error).message}`);
  }
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (error) {
    throw new Error(`the configuration ${file} is not JSON: ${(error as Error).message}`);
  }
  return parseConfig(value, `the configuration ${file}`);
}

/** The store the configuration names `name`; throws when it names none. */
export function storeNamed(config: SkillCutsConfig, name: string): Store {
  const store = config.stores.find((candidate) => candidate.name === name);
  if (store === undefined) {
    throw new Error(
      `the configuration names no store ${JSON.stringify(name)}; it names ${config.stores.map((s) => s.name).join(", ")}`
    );
  }
  return store;
}
