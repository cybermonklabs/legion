import { z } from "zod";
import type { GithubReads } from "./build";
import type { SkillCutsConfig } from "./config";

const SHA = /^[a-f0-9]{40}$/;
const DIGEST = /^sha256:[a-f0-9]{64}$/;
const REVISION = "org.opencontainers.image.revision";
const SOURCE = "org.opencontainers.image.source";
const IMAGE = /^(?:[\w.-]+\/)+[\w.-]+(?::[\w.-]+)?@(sha256:[a-f0-9]{64})$/;
const GHCR = /^ghcr\.io\/([a-z0-9._/-]+?)(?::[\w.-]+)?@(sha256:[a-f0-9]{64})$/;

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
}
export type ReadImageConfig = (image: string) => Promise<ImageConfig | undefined>;

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
});

/** Read a public GHCR image's config, not its layers. The reference must pin a digest. */
export async function registryImageConfig(image: string): Promise<ImageConfig | undefined> {
  const match = GHCR.exec(image);
  const [, repository, digest] = match ?? [];
  if (repository === undefined || digest === undefined) return undefined;
  if (repository.split("/").some((part) => part === "" || part === "." || part === ".."))
    return undefined;
  const signal = AbortSignal.timeout(30_000);
  const tokenUrl = new URL("https://ghcr.io/token");
  tokenUrl.searchParams.set("scope", `repository:${repository}:pull`);
  const get = async (url: string | URL, headers?: Record<string, string>): Promise<unknown> => {
    const response = await fetch(url, { headers, signal });
    if ([401, 403, 404].includes(response.status)) return undefined;
    if (!response.ok) throw new Error(`reading ${image}: registry HTTP ${response.status}`);
    return response.json();
  };
  const token = z.object({ token: z.string() }).safeParse(await get(tokenUrl));
  if (!token.success) return undefined;
  const headers = {
    Authorization: `Bearer ${token.data.token}`,
    Accept: [
      "application/vnd.oci.image.index.v1+json",
      "application/vnd.oci.image.manifest.v1+json",
      "application/vnd.docker.distribution.manifest.list.v2+json",
      "application/vnd.docker.distribution.manifest.v2+json",
    ].join(", "),
  };
  const root = `https://ghcr.io/v2/${repository}`;
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
    ? { labels: blob.data.config.Labels ?? {}, env: blob.data.config.Env ?? [] }
    : undefined;
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

interface FinalStage {
  readonly base: string | undefined;
  readonly instructions: readonly string[];
  readonly baseComment: string;
}

/** Read the final stage's literal FROM (or its ARG default) and instructions. */
function finalStage(dockerfile: string): FinalStage {
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
  const digest =
    typeof image === "string" ? IMAGE.exec(image)?.[1]?.slice("sha256:".length) : undefined;
  if (digest === undefined)
    throw new PodNotMeasurable(`${pinAt} has no pinned digest at ${pinKey}`);
  const pinLines = pin.split(/\r?\n/);
  const pinLine = pinLines.findIndex((line) => line.trimStart().startsWith(`${pinKey}:`));
  const comment = commentAbove(pinLines, pinLine);
  const attested = await github.attestedCommit(repository, digest);
  // The pin can move back to an older build. Read that build's Dockerfile, not the pin commit's.
  const ref =
    attested?.commit ?? /\b(?:built commit|push of commit) ([a-f0-9]{7,40})\b/.exec(comment)?.[1];
  if (ref === undefined)
    throw new PodNotMeasurable(
      `${pinAt}:${pinLine + 1} names no build commit for sha256:${digest}`
    );
  const build = ref.length === 40 ? ref : await github.commitAt(repository, ref, at);
  if (!build.startsWith(ref))
    throw new PodNotMeasurable(`${pinAt} names an unresolved build commit ${ref}`);
  const recipe = await github.text(repository, build, dockerfile);
  if (recipe === undefined) throw new PodNotMeasurable(`${dockerfile} at ${build} is missing`);
  const stage = finalStage(recipe);
  const base = stage.base;
  if (base === undefined || !IMAGE.test(base))
    throw new PodNotMeasurable(`${dockerfile} at ${build} has no digest-pinned final FROM`);
  const oci = await github.imageConfig(base);
  const source = oci?.labels[SOURCE];
  if (
    source !== undefined &&
    source.replace(/\/$/, "").toLowerCase() !==
      `https://github.com/${workerRepository.toLowerCase()}`
  ) {
    throw new PodNotMeasurable(
      `source label of ${base} does not match the configured worker repository`
    );
  }
  const revision = oci?.labels[REVISION];
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
  const podLinks = linksIn(profile, oci?.env.length ? oci.env : baseLinks.env, stage);
  const directories = [...new Set([...baseLinks.found, ...podLinks.found])];
  const why = `Inference: ${pinAt}:${pinLine + 1} selects the image in force at prompt build ${at}. ${attested === undefined ? "The pin comment" : "Build provenance"} names ${build}. ${dockerfile} at that commit uses ${base}. Its ${fromLabel ? REVISION : "Dockerfile pin comment"} names ${sha}. The Dockerfiles link ${directories.join(", ") || "no plugins"} into profile ${profile}.`;
  return { repo: workerRepository, sha, directories, why };
}
