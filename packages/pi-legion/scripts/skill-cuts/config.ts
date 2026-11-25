/**
 * The configuration of the skill-cuts runner: one JSON file, `skill-cuts.json`, passed to every
 * command as `--config <file>`, and `loadConfig` is the one loader every command uses. This
 * repository is public, so every private value the runner needs (the GitHub owners it lists, the
 * session stores it counts, the program that reaches them, the model gateway's address, the paths
 * it leaves out and the repositories its copies map to) comes from this file at run time and never
 * from a committed file. The real file is kept outside the repository.
 *
 * Keys (only the last six are optional):
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
 *   clones          (optional) `{repo, dir, clone}`: a clone made by hand on a dev box, a fact
 *                   about the host that no installer the copies output reads records, so the
 *                   operator states it here: the directory `dir` (a path in the repository, such
 *                   as `.secrets`) of the dev box's checkout of `repo` is a clone of the repository
 *                   `clone`. copies.ts reads a skill pool of `repo`'s skills-sources.json in that
 *                   directory as the clone's skills, beside the pools of the clones an installer
 *                   makes (`ensure_vendor`), the clone read as an installer's clone of no ref is:
 *                   its default branch at the time read, and each unit it gives names this key.
 *                   Both repositories must be of an owner in `owners`, and copies.ts refuses a
 *                   `dir` that holds no pool of `repo`'s skills-sources.json once it has read it.
 *   sessionPod     (optional) how to find a session pod's image: `repository` is the deployment
 *                  repository, `pinFile` is its image pin file, `pinKey` is the key under
 *                  `config`, and `dockerfile` is the session image's Dockerfile. `workerRepository`
 *                  and `workerDockerfile` name the base worker's source and Dockerfile.
 *   ompPin         (optional) where the dev box's own repository pins the Oh My Pi build it runs,
 *                  and the first build that loads an extension's resources_discover skill pools on
 *                  a fresh start: `file` a TOML file of that repository (a path in it, such as
 *                  `mise.toml`), `key` the keys that lead to the pinned version in it (`["tools",
 *                  "<tool>"]`), and `firstPoolVersion` that first build's version (semver, such as
 *                  `18.16.0` or `18.16.0-fork.1`). build.ts decides the pools of a session that
 *                  records no listing by the pin at the session's start against that version
 *                  (POOL_VERSION_RULE); without this key such pools are installed as the dev box's
 *                  installers install them.
 *   registries     (optional) `{host, tokenCommand}`: a registry the runner reads an image's
 *                  config from with a token (pod-image.ts, `registryImageConfig`; a public GHCR
 *                  image of a host not listed is read with the anonymous token GHCR gives): `host`
 *                  the registry's host (with its port, if any), `tokenCommand` the argv of a
 *                  program whose standard output, trimmed, is the registry's Bearer token. The
 *                  program runs once per process, and the token is kept in memory alone, never
 *                  printed.
 *   deploymentImages  (optional) `{repository, dockerfile}`: an image the deployment repository
 *                  builds on a worker image: `repository` the image's reference without tag or
 *                  digest, `dockerfile` the path in the deployment repository of the Dockerfile it
 *                  is built from. build.ts takes such an image's worker commit from the base that
 *                  Dockerfile names, once the images confirm it (LEGION_IMAGE_RULE).
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
/** A directory of a repository's checkout, as a path in the repository: `.secrets`,
 *  `vendor/pack`; not absolute, no `.` or `..` part, no `/` at its end. */
const CheckoutDirectory = z
  .string()
  .refine(
    (value) =>
      !value.startsWith("~") &&
      value.split("/").every((part) => part !== "" && part !== "." && part !== ".."),
    { error: "a directory of the checkout as a path in the repository, such as .secrets" }
  );
/** An instant in UTC, as the window's ends are written: `2026-10-23T00:00:00Z`. */
const Instant = z.iso.datetime({ error: "an ISO instant ending in Z" });
/** A version as a pin or a build names it, semver with an optional `v`: `18.16.0`,
 *  `18.16.0-fork.20261104-021732`. build.ts reads a pinned Oh My Pi version by it. */
export const OMP_VERSION = /^v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/;

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

/** A clone made by hand that the configuration states (the key `clones`): the directory `dir` of
 *  the dev box's checkout of `repo` is a clone of `clone`. */
export const Clone = z.strictObject({
  repo: Repository,
  dir: CheckoutDirectory,
  clone: Repository,
});
export type Clone = z.infer<typeof Clone>;

/** A registry the runner reads with a token (the key `registries`): its host, and the argv of
 *  the program that prints the token. */
export const Registry = z.strictObject({
  host: z.string().regex(/^[A-Za-z0-9.-]+(?::\d+)?$/, {
    error: "a registry host, such as registry.example.test",
  }),
  tokenCommand: z.array(z.string().min(1)).min(1),
});
export type Registry = z.infer<typeof Registry>;

/** An image the deployment repository builds on a worker image (the key `deploymentImages`): its
 *  reference without tag or digest, and its Dockerfile's path in the deployment repository. */
export const DeploymentImage = z.strictObject({
  repository: z.string().regex(/^[a-z0-9.-]+(?::\d+)?(?:\/[a-z0-9._-]+)+$/, {
    error: "an image reference without tag or digest, such as registry.example.test/acme/worker",
  }),
  dockerfile: CheckoutDirectory,
});
export type DeploymentImage = z.infer<typeof DeploymentImage>;

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
    clones: z.array(Clone).optional(),
    sessionPod: z
      .strictObject({
        repository: Repository,
        pinFile: CheckoutDirectory,
        pinKey: z.string().regex(/^[A-Za-z0-9_.:-]+$/),
        dockerfile: CheckoutDirectory,
        workerRepository: Repository,
        workerDockerfile: CheckoutDirectory,
      })
      .optional(),
    ompPin: z
      .strictObject({
        file: CheckoutDirectory,
        key: z.array(z.string().min(1)).min(1),
        firstPoolVersion: z.string().regex(OMP_VERSION, {
          error: "a version, such as 18.16.0 or 18.16.0-fork.1",
        }),
      })
      .optional(),
    registries: z.array(Registry).optional(),
    deploymentImages: z.array(DeploymentImage).optional(),
  })
  .refine((config) => Date.parse(config.windowStart) < Date.parse(config.windowEnd), {
    error: "windowEnd must be after windowStart",
    path: ["windowEnd"],
  })
  .superRefine((config, context) => {
    // A clone's repositories are read from the owners' listings, so each is of an owner in
    // `owners`; GitHub reads an owner's name in any case.
    const owners = new Set(config.owners.map((owner) => owner.toLowerCase()));
    for (const [index, clone] of (config.clones ?? []).entries()) {
      for (const key of ["repo", "clone"] as const) {
        const owner = clone[key].split("/")[0]?.toLowerCase() ?? "";
        if (owners.has(owner)) continue;
        context.addIssue({
          code: "custom",
          message: `${clone[key]} is not a repository of an owner in owners`,
          path: ["clones", index, key],
        });
      }
    }
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
