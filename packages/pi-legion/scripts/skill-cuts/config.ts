/**
 * The configuration of the skill-cuts runner: one JSON file, `skill-cuts.json`, passed to every
 * command as `--config <file>`, and `loadConfig` is the one loader every command uses. This
 * repository is public, so every private value the runner needs (the GitHub owners it lists, the
 * session stores it counts, the program that reaches them, the model gateway's address, the paths
 * it leaves out and the repositories its copies map to) comes from this file at run time and never
 * from a committed file. The real file is kept outside the repository.
 *
 * Keys, every one required but those marked optional, no other allowed:
 *   owners          GitHub owners whose repositories the copy lister reads
 *   stores          the session stores the count reads: `{name, context}`, `name` the database
 *                   name, `context` `devbox` or `legion:<PROJECT>` (whose sessions the store holds)
 *   psqlCommand     absolute path of a program that runs the store's read command for the database
 *                   its first argument names, with the query on its standard input
 *   transcriptStore (optional) `{command, hosts}`: an absolute program path that reads a
 *                   ClickHouse query on stdin and prints its rows on stdout, and the
 *                   `{host, context}` pairs for the hosts whose contexts are known.
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
 *   devboxHome     (optional) the dev box's home directory, an absolute path. build.ts gives a
 *                  named-profile session of a devbox store with a `[Skill file: …]` header under
 *                  it (by whole path segments) the dev box's context: such a session ran in a pod
 *                  that mounts that home (SESSION_CONTEXT_RULE). Without this key every
 *                  named-profile session of a devbox store gets the session pod's context. It
 *                  names a person's home on the dev box, a private value that only this file holds.
 *   sets            (optional) `{id, repo}`: each skill set `set-count` measures against our
 *                   skills, which `attribute --changed`, `build-cases --set` and `build-cases
 *                   --reprofile` take where they take a cut (`cutOrSet`). `id` is the set's name,
 *                   which is also its Claude Code plugin name and the `<id>:` prefix of its skills
 *                   in a Claude Code `Skill` call; no cut may have it as its id, and no two sets.
 *                   `repo` is the set's upstream repository (`owner/name`).
 *   dotfilesRepo    (optional; required when `sets` is given) the repository (`owner/name`) whose
 *                   `omp/plugins/package.json` and `installers/omp.sh` pin the sets (sets.ts)
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
 *                  program runs once per process, ended past a minute
 *                  (REGISTRY_TOKEN_TIMEOUT_MS), and the token is kept in memory alone, never
 *                  printed.
 *   deploymentImages  (optional) `{repository, dockerfile, readSteps}`: an image the deployment
 *                  repository builds on a worker image: `repository` the image's reference without
 *                  tag or digest, `dockerfile` the path in the deployment repository of the
 *                  Dockerfile it is built from. build.ts takes such an image's worker commit from
 *                  the stage that Dockerfile's final stage copies the worker's packages from, once
 *                  the image's version label names that stage's commit, else, where it copies none,
 *                  from the base it names, once the images confirm it (LEGION_IMAGE_RULE).
 *                  `readSteps` (optional) `{sha256, reason}`: a RUN step of that final stage that a
 *                  person read and found to write no copied package: `sha256` the SHA-256, as 64
 *                  lower-case hex digits, of the step's text in UTF-8 as pod-image.ts's
 *                  `dockerfileStages` gives it in `steps` (the instruction from its `RUN` on, its
 *                  comment lines left out, a line continued with `\` joined to the next, a
 *                  heredoc's lines read with it, each run of white space one space, none at its
 *                  ends), at the Dockerfile revision the image's label names; `reason` what the
 *                  person read in the step and why it writes no copied package. A RUN after a
 *                  package's last COPY that names a path of it is not taken as a write of it where
 *                  its hash is listed; a step whose text changed has another hash. Each hash is
 *                  listed once in an entry.
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
/** Whether `value` is a path in a repository: not absolute, not from `~`, no empty, `.` or `..`
 *  part, so no `/` at its end. */
const inRepository = (value: string) =>
  !value.startsWith("~") &&
  value.split("/").every((part) => part !== "" && part !== "." && part !== "..");
/** A directory of a repository's checkout, as a path in the repository: `.secrets`,
 *  `vendor/pack` (`inRepository`). */
const CheckoutDirectory = z.string().refine(inRepository, {
  error: "a directory of the checkout as a path in the repository, such as .secrets",
});
/** A file of a repository, as a path in the repository: `mise.toml`, `pods/Dockerfile`
 *  (`inRepository`). */
const RepositoryFile = z.string().refine(inRepository, {
  error: "a file of the repository as a path in the repository, such as mise.toml",
});
/** An instant in UTC, as the window's ends are written: `2026-10-23T00:00:00Z`. */
const Instant = z.iso.datetime({ error: "an ISO instant ending in Z" });
/** A skill set's name: a directory name under set-count's `--out` and a `<name>:` skill prefix. */
const SetId = z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._-]*$/, {
  error: "a set name of letters, digits, ., _ and -",
});
/** A version as a pin or a build names it, semver with an optional `v`: `18.16.0`,
 *  `18.16.0-fork.20261104-021732`. build.ts reads a pinned Oh My Pi version by it. */
export const OMP_VERSION = /^v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/;

const StoreContext = z.string().regex(/^(devbox|legion:[A-Z][A-Z0-9]*)$/, {
  error: "devbox or legion:<PROJECT>",
});
export const TranscriptHost = z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._-]*$/, {
  error: "a host of letters, digits, dots, hyphens or underscores",
});
/** Transcript query directory and the prefix of each host's store label. Not a database name. */
export const TRANSCRIPT_SOURCE = "transcripts";
export const transcriptStoreName = (host: string) => `${TRANSCRIPT_SOURCE}@${host}`;

/** A session store: its database name, which a query names only as a value, so it is held to the
 *  letters a database identifier needs, and whose sessions it holds. */
export const Store = z.strictObject({
  name: z.string().regex(/^[a-z_][a-z0-9_]*$/, {
    error: "a database name of small letters, digits and _",
  }),
  context: StoreContext,
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

/** A RUN step of a deployment image's final stage that a person read and found to write no copied
 *  package (the key `readSteps` of a `deploymentImages` entry): the SHA-256 of its text, as 64
 *  lower-case hex digits, and what the person read in it. */
export const ReadStep = z.strictObject({
  sha256: z.string().regex(/^[0-9a-f]{64}$/, {
    error: "the SHA-256 of a step's text as 64 lower-case hex digits",
  }),
  reason: z.string().min(1, { error: "what a person read in the step, not empty" }),
});
export type ReadStep = z.infer<typeof ReadStep>;

/** An image the deployment repository builds on a worker image (the key `deploymentImages`): its
 *  reference without tag or digest, its Dockerfile's path in the deployment repository, and the
 *  RUN steps of its final stage that a person read (`ReadStep`). */
export const DeploymentImage = z.strictObject({
  repository: z.string().regex(/^[a-z0-9.-]+(?::\d+)?(?:\/[a-z0-9._-]+)+$/, {
    error: "an image reference without tag or digest, such as registry.example.test/acme/worker",
  }),
  dockerfile: RepositoryFile,
  readSteps: z.array(ReadStep).optional(),
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
    transcriptStore: z
      .strictObject({
        command: z
          .string()
          .refine((value) => path.isAbsolute(value), { error: "an absolute path" }),
        hosts: z.array(z.strictObject({ host: TranscriptHost, context: StoreContext })),
      })
      .optional(),
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
    devboxHome: z
      .string()
      .refine((value) => path.isAbsolute(value), { error: "an absolute path" })
      .optional(),
    sets: z.array(z.strictObject({ id: SetId, repo: Repository })).optional(),
    dotfilesRepo: Repository.optional(),
    clones: z.array(Clone).optional(),
    sessionPod: z
      .strictObject({
        repository: Repository,
        pinFile: RepositoryFile,
        pinKey: z.string().regex(/^[A-Za-z0-9_.:-]+$/),
        dockerfile: RepositoryFile,
        workerRepository: Repository,
        workerDockerfile: RepositoryFile,
      })
      .optional(),
    ompPin: z
      .strictObject({
        file: RepositoryFile,
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
  .superRefine((config, ctx) => {
    if (config.sets === undefined) return;
    if (config.dotfilesRepo === undefined) {
      ctx.addIssue({
        code: "custom",
        path: ["dotfilesRepo"],
        message: "required when sets is given",
      });
    }
    const cuts = new Set(config.cuts.map((cut) => cut.id));
    const seen = new Set<string>();
    for (const [index, set] of config.sets.entries()) {
      const id = JSON.stringify(set.id);
      if (cuts.has(set.id)) {
        ctx.addIssue({
          code: "custom",
          path: ["sets", index, "id"],
          message: `${id} is the id of a cut; a set id may not equal a cut id`,
        });
      }
      if (seen.has(set.id)) {
        ctx.addIssue({
          code: "custom",
          path: ["sets", index, "id"],
          message: `${id} is given twice`,
        });
      }
      seen.add(set.id);
    }
  })
  .superRefine((config, context) => {
    const hosts = new Set<string>();
    for (const [index, { host }] of (config.transcriptStore?.hosts ?? []).entries()) {
      if (hosts.has(host)) {
        context.addIssue({
          code: "custom",
          message: "a host listed twice",
          path: ["transcriptStore", "hosts", index, "host"],
        });
      }
      hosts.add(host);
    }
    if (
      config.transcriptStore !== undefined &&
      config.stores.some(({ name }) => name === TRANSCRIPT_SOURCE)
    ) {
      context.addIssue({
        code: "custom",
        message: `${TRANSCRIPT_SOURCE} is reserved for the transcript store`,
        path: ["stores"],
      });
    }
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
    for (const [image, { readSteps }] of (config.deploymentImages ?? []).entries()) {
      const hashes = new Set<string>();
      for (const [index, { sha256 }] of (readSteps ?? []).entries()) {
        if (hashes.has(sha256)) {
          context.addIssue({
            code: "custom",
            message: "a step hash listed twice",
            path: ["deploymentImages", image, "readSteps", index, "sha256"],
          });
        }
        hashes.add(sha256);
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

/** A cut or a skill set of the configuration, by its id, with its repository: what `attribute
 *  --changed`, `build-cases` and `build-cases --reprofile` measure. */
export interface CutOrSet {
  readonly kind: "cut" | "set";
  readonly id: string;
  readonly repo: string;
}

/** The cut or the set whose id is `id`, or undefined when the configuration has neither. A set id
 *  is never a cut id (SkillCutsConfig refuses one), so at most one matches. */
export function cutOrSet(config: SkillCutsConfig, id: string): CutOrSet | undefined {
  const cut = config.cuts.find((each) => each.id === id);
  if (cut !== undefined) return { kind: "cut", id: cut.id, repo: cut.repo };
  const set = config.sets?.find((each) => each.id === id);
  return set === undefined ? undefined : { kind: "set", id: set.id, repo: set.repo };
}
