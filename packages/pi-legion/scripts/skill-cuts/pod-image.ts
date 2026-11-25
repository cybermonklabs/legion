import { z } from "zod";
import type { GithubReads } from "./build";
import {
  fetchFailure,
  type GhRetries,
  type GhTry,
  httpFailure,
  lastTryError,
  pastLimit,
  textOf,
  timeLimit,
} from "./common";
import type { Registry, SkillCutsConfig } from "./config";

const SHA = /^[a-f0-9]{40}$/;
const DIGEST = /^sha256:[a-f0-9]{64}$/;
const REVISION = "org.opencontainers.image.revision";
const SOURCE = "org.opencontainers.image.source";
/** A digest-pinned reference: its host (with its port, if any), its path and its digest (group
 *  3). The path is held to lower case, as an image reference's path is, so a pinned image or a
 *  base with an upper-case letter in its path is refused as unpinned. */
const REFERENCE =
  /^([A-Za-z0-9.-]+(?::\d+)?)\/([a-z0-9._/-]+?)(?::[\w.-]+)?@(sha256:[a-f0-9]{64})$/;
/** The longest a registry's tokenCommand may run before it is ended: the command prints a token it
 *  holds or asks its issuer for, so a minute is room for a slow issuer, and a command that hangs
 *  holds no read, and no later read of that registry, past it. */
export const REGISTRY_TOKEN_TIMEOUT_MS = 60_000;
/** The longest one HTTP request of an image's read (the token GHCR gives, a manifest, the config
 *  blob) may take, its body included, before it is given up and sent again: a fetch has no time
 *  limit of its own, and a read of one image under load on the pod took 23.6 s in all (measured),
 *  so each request has room for a slow answer. The token command's limit is its own, not in it. */
const REGISTRY_REQUEST_TIMEOUT_MS = 30_000;

export class PodNotMeasurable extends Error {
  constructor(why: string) {
    super(`pod profile not measurable: ${why}`);
  }
}

export interface PodImage {
  readonly repo: string;
  readonly sha: string;
  readonly directories: readonly string[];
  readonly why: string;
}

export interface ImageConfig {
  readonly labels: Readonly<Record<string, string>>;
  readonly env: readonly string[];
  /** The config blob's `rootfs.diff_ids`: the digest of each layer uncompressed, in order. */
  readonly diffIds: readonly string[];
}
export type ReadImageConfig = (image: string) => Promise<ImageConfig | undefined>;

/** One HTTP GET of `url` (`fetch` in a real run). */
export type RegistryFetch = (
  url: string,
  init: { readonly headers?: Record<string, string>; readonly signal: AbortSignal }
) => Promise<Response>;

/** What `registryImageConfig` reads a registry with: the rule by which a request that failed for
 *  a transient reason is sent again, which counts and says each retry (`retries`: the profile's
 *  GitHub reads' own in a real run, build.ts's `githubReads`), the registries the configuration
 *  names (`registries`), the fetch it sends (`fetch` when absent), the time limit of a registry's
 *  tokenCommand (REGISTRY_TOKEN_TIMEOUT_MS when absent) and that of each request
 *  (REGISTRY_REQUEST_TIMEOUT_MS when absent). */
export interface RegistryAccess {
  readonly retries: GhRetries;
  readonly registries?: readonly Registry[];
  readonly fetch?: RegistryFetch;
  readonly tokenTimeoutMs?: number;
  readonly requestTimeoutMs?: number;
}

/** One try of a registry's request, as `GhRetries.send` reads it: code 0 with the answer's JSON
 *  (none for an answer that names no image), else code 1 with its failure in common.ts's words
 *  (`httpFailure`, `fetchFailure`); `status` is the answer's HTTP status, none when none came. */
interface RegistryTry extends GhTry {
  readonly status?: number;
  readonly json?: unknown;
}

const Manifest = z.object({
  config: z.object({ digest: z.string().regex(DIGEST) }).optional(),
  manifests: z
    .array(
      z.object({
        digest: z.string().regex(DIGEST),
        platform: z.object({ os: z.string(), architecture: z.string() }).optional(),
      })
    )
    .optional(),
});
const ConfigBlob = z.object({
  config: z.object({
    Labels: z.record(z.string(), z.string()).nullish(),
    Env: z.array(z.string()).nullish(),
  }),
  rootfs: z.object({ diff_ids: z.array(z.string()).nullish() }).nullish(),
});

/** The token of each registry's tokenCommand, by its argv: the command runs once per process. */
const tokens = new Map<string, Promise<string>>();

/** The trimmed standard output of `registry`'s tokenCommand, run once per process, and ended once
 *  it runs past `timeoutMs` (common.ts's `timeLimit`). Its failure names the host and the exit
 *  status or the limit, never the command or what it printed. */
function registryToken(registry: Registry, timeoutMs: number): Promise<string> {
  const key = JSON.stringify(registry.tokenCommand);
  const known = tokens.get(key);
  if (known !== undefined) return known;
  const token = (async () => {
    const child = Bun.spawn([...registry.tokenCommand], {
      stdin: "ignore",
      stdout: "pipe",
      stderr: "ignore",
    });
    const limit = timeLimit(child, timeoutMs);
    const [out, code] = await Promise.all([textOf(child.stdout, limit), child.exited]);
    limit.clear();
    const command = `the token command of ${registry.host}`;
    if (limit.timedOut()) throw new Error(pastLimit(command, timeoutMs));
    if (code !== 0) throw new Error(`${command} exited ${code}`);
    const trimmed = out.trim();
    if (trimmed === "") throw new Error(`${command} printed no token`);
    return trimmed;
  })();
  tokens.set(key, token);
  return token;
}

/** Read an image's config, not its layers, from its registry: a registry the configuration names
 *  (`access.registries`) with the Bearer token its tokenCommand prints, else a public GHCR image
 *  with the anonymous pull token GHCR gives; undefined for any other host, and for an image the
 *  registry does not have (a 404; on GHCR's anonymous read a 401 or 403 too: no public image). The
 *  reference must pin a digest. Each request is given up after its own time limit, its body
 *  included, and one that failed for a transient reason (that limit, a connection that failed, an
 *  HTTP 429 or 5xx) is sent again by the runner's one rule (`access.retries`, common.ts's
 *  `GhRetries`); after its last try the read fails naming the image and that try's failure. A
 *  configured registry's 401 or 403 is a refusal of its token: the read fails at once, naming the
 *  host, the image and the status. The token command runs outside every request's limit. No
 *  message names the token. */
export async function registryImageConfig(
  image: string,
  access: RegistryAccess
): Promise<ImageConfig | undefined> {
  const [, host, repository, digest] = REFERENCE.exec(image) ?? [];
  if (host === undefined || repository === undefined || digest === undefined) return undefined;
  if (repository.split("/").some((part) => part === "" || part === "." || part === ".."))
    return undefined;
  const registry = access.registries?.find((each) => each.host === host);
  if (registry === undefined && host !== "ghcr.io") return undefined;
  const send = access.fetch ?? ((url, init) => fetch(url, init));
  const limitMs = access.requestTimeoutMs ?? REGISTRY_REQUEST_TIMEOUT_MS;
  const get = async (url: string, headers?: Record<string, string>): Promise<unknown> => {
    let tries = 0;
    const read = await access.retries.send(
      async (): Promise<RegistryTry> => {
        tries++;
        const signal = AbortSignal.timeout(limitMs);
        try {
          const response = await send(url, { headers, signal });
          const { status } = response;
          if (status === 404 || (registry === undefined && (status === 401 || status === 403)))
            return { code: 0, stderr: "", status };
          if (!response.ok) return { code: 1, stderr: httpFailure(status), status };
          return { code: 0, stderr: "", status, json: await response.json() };
        } catch (error) {
          const failure = fetchFailure(error, signal, limitMs);
          if (failure === undefined) throw error;
          return { code: 1, stderr: failure };
        }
      },
      { read: `the registry read of ${url}` }
    );
    if (read.code === 0) return read.json;
    if (read.status === 401 || read.status === 403) {
      throw new Error(
        `reading ${image}: ${host} refused the token of its tokenCommand (HTTP ${read.status})`
      );
    }
    const last = lastTryError(read);
    throw new Error(
      `reading ${image}: registry ${last}${tries > 1 ? ` on the last of ${tries} tries` : ""}`
    );
  };
  let token: string;
  if (registry === undefined) {
    const tokenUrl = new URL("https://ghcr.io/token");
    tokenUrl.searchParams.set("scope", `repository:${repository}:pull`);
    const anonymous = z.object({ token: z.string() }).safeParse(await get(tokenUrl.href));
    if (!anonymous.success) return undefined;
    token = anonymous.data.token;
  } else {
    token = await registryToken(registry, access.tokenTimeoutMs ?? REGISTRY_TOKEN_TIMEOUT_MS);
  }
  const headers = {
    Authorization: `Bearer ${token}`,
    Accept: [
      "application/vnd.oci.image.index.v1+json",
      "application/vnd.oci.image.manifest.v1+json",
      "application/vnd.docker.distribution.manifest.list.v2+json",
      "application/vnd.docker.distribution.manifest.v2+json",
    ].join(", "),
  };
  const root = `https://${host}/v2/${repository}`;
  const manifest = async (id: string) =>
    Manifest.safeParse(await get(`${root}/manifests/${id}`, headers));
  let found = (await manifest(digest)).data;
  if (found?.manifests !== undefined) {
    const matches = found.manifests.filter(
      ({ platform }) => platform?.os === "linux" && platform.architecture === "amd64"
    );
    const only = matches.length === 1 ? matches[0] : undefined;
    found = only === undefined ? undefined : (await manifest(only.digest)).data;
  }
  if (found?.config === undefined) return undefined;
  const blob = ConfigBlob.safeParse(await get(`${root}/blobs/${found.config.digest}`, headers));
  return blob.success
    ? {
        labels: blob.data.config.Labels ?? {},
        env: blob.data.config.Env ?? [],
        diffIds: blob.data.rootfs?.diff_ids ?? [],
      }
    : undefined;
}

/**
 * Whether the image `base` is the start of the image `image`, by their configs' `rootfs.diff_ids`
 * (each layer's digest uncompressed): `base` holds some, and they are the first of `image`'s.
 * The manifests' layer digests are of the compressed layers, which differ where a build
 * compressed a base layer again, so they decide nothing. `words` say so, or why not, naming both
 * images.
 */
export function builtOn(
  base: { readonly name: string; readonly diffIds: readonly string[] },
  image: { readonly name: string; readonly diffIds: readonly string[] }
): { readonly on: boolean; readonly words: string } {
  const n = base.diffIds.length;
  const m = image.diffIds.length;
  if (n === 0) return { on: false, words: `${base.name} holds no rootfs.diff_ids` };
  if (m === 0) return { on: false, words: `${image.name} holds no rootfs.diff_ids` };
  const counts = `the ${n} rootfs.diff_ids of ${base.name}`;
  if (n > m) return { on: false, words: `${counts} are more than the ${m} of ${image.name}` };
  const differs = base.diffIds.findIndex((id, at) => id !== image.diffIds[at]);
  if (differs >= 0) {
    return {
      on: false,
      words: `${counts} are not the first ${n} of the ${m} of ${image.name} (entry ${differs + 1} differs)`,
    };
  }
  return { on: true, words: `${counts} are the first ${n} of the ${m} of ${image.name}` };
}

function commentAbove(lines: readonly string[], index: number): string {
  const comment: string[] = [];
  for (let n = index - 1; n >= 0; n--) {
    const line = lines[n]?.trimStart();
    if (!line?.startsWith("#")) break;
    comment.unshift(line.slice(1).trim());
  }
  return comment.join(" ");
}

export interface FinalStage {
  readonly base: string | undefined;
  readonly instructions: readonly string[];
  readonly baseComment: string;
}

/** Read the final stage's literal FROM (or its ARG default) and instructions. */
export function finalStage(dockerfile: string): FinalStage {
  const lines = dockerfile.split(/\r?\n/);
  const at = lines.findLastIndex((line) => /^\s*FROM\s/i.test(line));
  const from = /^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)/i.exec(lines[at] ?? "")?.[1];
  const arg = /^\$\{?(\w+)\}?$/.exec(from ?? "")?.[1];
  const argLine = lines.findIndex(
    (line) => /^\s*ARG\s+(\w+)=/.exec(line)?.[1] === arg && arg !== undefined
  );
  const base = arg === undefined ? from : /^\s*ARG\s+\w+=(\S+)/.exec(lines[argLine] ?? "")?.[1];
  const body = lines
    .slice(at + 1)
    .filter((line) => !line.trimStart().startsWith("#"))
    .join("\n");
  const instructions = body.replace(/\\\n/g, " ").split("\n");
  return { base, instructions, baseComment: commentAbove(lines, argLine < 0 ? at : argLine) };
}

/** Associate each link with the profile set when its RUN executes. */
function linksIn(profile: string, env: readonly string[], stage: FinalStage) {
  const variables = new Map(
    env.map((entry) => {
      const eq = entry.indexOf("=");
      return [entry.slice(0, eq), entry.slice(eq + 1)];
    })
  );
  const found: string[] = [];
  for (const line of stage.instructions) {
    if (/^\s*ENV\s/i.test(line)) {
      for (const [, name, quoted, bare] of line.matchAll(
        /\b(OMP_PROFILE|PI_PROFILE)=(?:"([^"]*)"|(\S*))/g
      )) {
        if (name !== undefined) variables.set(name, quoted ?? bare ?? "");
      }
    }
    const active = (variables.get("OMP_PROFILE") ?? variables.get("PI_PROFILE") ?? "").trim();
    if ((active || "default") !== profile || !/^\s*RUN\s/i.test(line)) continue;
    for (const match of line.matchAll(/\bomp plugin link\s+['"]?(\/[\w./@-]+)(?=[\s;'"&]|$)/g)) {
      if (match[1] !== undefined) found.push(match[1]);
    }
  }
  return { found, env: [...variables].map(([name, value]) => `${name}=${value}`) };
}

/** Return locations of packages linked into this profile and the immutable tree that built them. */
export async function sessionPodImage(
  github: Pick<
    GithubReads,
    "githubBranch" | "commitAt" | "text" | "attestedCommit" | "imageConfig"
  >,
  config: NonNullable<SkillCutsConfig["sessionPod"]>,
  profile: string,
  at: string
): Promise<PodImage> {
  const { repository, pinFile, pinKey, dockerfile, workerRepository, workerDockerfile } = config;
  const { branch } = await github.githubBranch(repository);
  const pinCommit = await github.commitAt(repository, branch, at);
  const pin = await github.text(repository, pinCommit, pinFile);
  const pinAt = `${pinFile} at ${pinCommit}`;
  if (pin === undefined) throw new PodNotMeasurable(`${pinAt} is missing`);
  const stack = z
    .object({ config: z.record(z.string(), z.unknown()) })
    .safeParse(Bun.YAML.parse(pin));
  const image = stack.data?.config[pinKey];
  const pinned = typeof image === "string" ? image : "";
  const digest = REFERENCE.exec(pinned)?.[3]?.slice("sha256:".length);
  if (digest === undefined)
    throw new PodNotMeasurable(`${pinAt} has no pinned digest at ${pinKey}`);
  const pinLines = pin.split(/\r?\n/);
  const pinLine = pinLines.findIndex((line) => line.trimStart().startsWith(`${pinKey}:`));
  // A key the YAML reads that no line starts with (a quoted key, say) has no line to cite, and no
  // comment above it (commentAbove of -1 is empty).
  const pinPlace =
    pinLine === -1 ? `${pinAt} (no line of it starts with ${pinKey}:)` : `${pinAt}:${pinLine + 1}`;
  const comment = commentAbove(pinLines, pinLine);
  const attested = await github.attestedCommit(repository, digest);
  // The pin can move back to an older build. Read that build's Dockerfile, not the pin commit's.
  const ref =
    attested?.commit ?? /\b(?:built commit|push of commit) ([a-f0-9]{7,40})\b/.exec(comment)?.[1];
  if (ref === undefined)
    throw new PodNotMeasurable(`${pinPlace} names no build commit for sha256:${digest}`);
  const build = ref.length === 40 ? ref : await github.commitAt(repository, ref, at);
  if (!build.startsWith(ref))
    throw new PodNotMeasurable(`${pinAt} names an unresolved build commit ${ref}`);
  const recipe = await github.text(repository, build, dockerfile);
  if (recipe === undefined) throw new PodNotMeasurable(`${dockerfile} at ${build} is missing`);
  const stage = finalStage(recipe);
  const base = stage.base;
  if (base === undefined || !REFERENCE.test(base))
    throw new PodNotMeasurable(`${dockerfile} at ${build} has no digest-pinned final FROM`);
  const oci = await github.imageConfig(base);
  if (oci === undefined)
    throw new PodNotMeasurable(
      `the runner cannot read the OCI config of ${base}, the base ${dockerfile} at ${build} names, from its registry, so it cannot confirm that the pinned image sha256:${digest} is built on it`
    );
  const source = oci.labels[SOURCE];
  if (
    source !== undefined &&
    source.replace(/\/$/, "").toLowerCase() !==
      `https://github.com/${workerRepository.toLowerCase()}`
  ) {
    throw new PodNotMeasurable(
      `source label of ${base} does not match the configured worker repository`
    );
  }
  // A build can pass the Dockerfile another base than its ARG default, so the images confirm the
  // base: its layers are the first of the pinned image's.
  const pinnedConfig = await github.imageConfig(pinned);
  if (pinnedConfig === undefined)
    throw new PodNotMeasurable(
      `the runner cannot read the OCI config of the pinned image sha256:${digest} (${pinned}) from its registry, so it cannot confirm that the image is built on ${base}, the base ${dockerfile} at ${build} names`
    );
  const layers = builtOn(
    { name: base, diffIds: oci.diffIds },
    { name: pinned, diffIds: pinnedConfig.diffIds }
  );
  if (!layers.on)
    throw new PodNotMeasurable(
      `${dockerfile} at ${build} names the base ${base}, but ${layers.words}, so the pinned image sha256:${digest} is not built on it`
    );
  const revision = oci.labels[REVISION];
  const fromLabel = revision !== undefined && SHA.test(revision);
  const historical = /\b(?:tag commit|built from commit) ([a-f0-9]{40})\b/.exec(
    stage.baseComment
  )?.[1];
  const sha = fromLabel ? revision : historical;
  if (sha === undefined)
    throw new PodNotMeasurable(
      `${dockerfile} at ${build} pins ${base}, which has no OCI revision or commit in the pin history`
    );
  const workerRecipe = await github.text(workerRepository, sha, workerDockerfile);
  if (workerRecipe === undefined)
    throw new PodNotMeasurable(`${workerDockerfile} at ${sha} is missing`);
  const worker = finalStage(workerRecipe);
  const baseLinks = linksIn(profile, [], worker);
  const podLinks = linksIn(profile, oci.env.length ? oci.env : baseLinks.env, stage);
  const directories = [...new Set([...baseLinks.found, ...podLinks.found])];
  const why = `Inference: ${pinPlace} selects the image in force at prompt build ${at}. ${attested === undefined ? "The pin comment" : "Build provenance"} names ${build}. ${dockerfile} at that commit uses ${base}; ${layers.words}, so the pinned image is built on it. Its ${fromLabel ? REVISION : "Dockerfile pin comment"} names ${sha}. The Dockerfiles link ${directories.join(", ") || "no plugins"} into profile ${profile}.`;
  return { repo: workerRepository, sha, directories, why };
}
