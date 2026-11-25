import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { GhRetries } from "./common";
import {
  type ImageConfig,
  type RegistryFetch,
  registryImageConfig,
  sessionPodImage,
} from "./pod-image";

// registryImageConfig on made-up registries: a stand-in fetch answers each read, and nothing
// reaches a network. Every host, image and token here is made up.

const scratch = temporaryDirectory("skill-cuts-pod-image", { real: true });

const INDEX = `sha256:${"a".repeat(64)}`;
const AMD64 = `sha256:${"d".repeat(64)}`;
const ARM64 = `sha256:${"e".repeat(64)}`;
const CONFIG = `sha256:${"c".repeat(64)}`;
const TOKEN = "tok-made-up-1";
const LAYERS = [1, 2, 3].map((n) => `sha256:${String(n).repeat(64)}`);

/** How long a stand-in read that never answers waits for its request's own time limit before the
 *  stand-in ends it itself, so a request with no limit of its own fails its row in seconds, naming
 *  why, rather than holding it. A real timer: the limit under test is the read's own
 *  `AbortSignal.timeout`, which no fake clock ends. */
const HANG_GUARD_MS = 2_000;

/** A stand-in registry's answer to one try: an HTTP status with no body, or `hang`, an answer that
 *  never comes, which only the request's own time limit ends. */
type Answer = number | "hang";

/** A stand-in fetch over `routes` (the JSON body of each URL; a 404 for any other URL), whose
 *  `n`-th try (from 0) of `url` is answered by `answer(url, n)` instead when it gives one, and the
 *  URL and Authorization header of each read it took. */
function standinFetch(
  routes: Readonly<Record<string, unknown>>,
  answer: (url: string, n: number) => Answer | undefined = () => undefined
) {
  const reads: { url: string; authorization: string | undefined }[] = [];
  const send: RegistryFetch = async (url, init) => {
    const n = reads.filter((read) => read.url === url).length;
    reads.push({ url, authorization: init.headers?.Authorization });
    const given = answer(url, n);
    if (given === "hang") {
      const { promise, reject } = Promise.withResolvers<Response>();
      const guard = setTimeout(
        () => reject(new Error(`the read of ${url} had no time limit of its own`)),
        HANG_GUARD_MS
      );
      init.signal.addEventListener(
        "abort",
        () => {
          clearTimeout(guard);
          reject(init.signal.reason);
        },
        { once: true }
      );
      return promise;
    }
    if (given !== undefined) return new Response(null, { status: given });
    if (!Object.hasOwn(routes, url)) return new Response("not found", { status: 404 });
    return Response.json(routes[url]);
  };
  return { reads, send };
}

/** The runner's retry rule (common.ts's GhRetries) as build-cases holds it, sending a read again
 *  with no wait: each wait it asks for kept in `waits`, each line it says in `lines`. */
function retryRule() {
  const waits: number[] = [];
  const lines: string[] = [];
  const retries = new GhRetries(
    { sleep: async (ms) => void waits.push(ms), err: (line) => void lines.push(line) },
    "build-cases"
  );
  return { retries, waits, lines };
}

/** The registry's answers for the image `<root>@INDEX`: an index of two platforms, the linux/amd64
 *  manifest, and its config blob. */
const imageRoutes = (root: string): Record<string, unknown> => ({
  [`${root}/manifests/${INDEX}`]: {
    manifests: [
      { digest: ARM64, platform: { os: "linux", architecture: "arm64" } },
      { digest: AMD64, platform: { os: "linux", architecture: "amd64" } },
    ],
  },
  [`${root}/manifests/${AMD64}`]: { config: { digest: CONFIG } },
  [`${root}/blobs/${CONFIG}`]: {
    config: {
      Labels: { "org.opencontainers.image.revision": "de".repeat(20) },
      Env: ["OMP_PROFILE=legion"],
    },
    rootfs: { type: "layers", diff_ids: LAYERS },
  },
});

// Catches: an image in a private registry, built by the deployment on a worker image, whose config
// the runner never read (it read public GHCR images alone), so a case on it was not measurable.
test("registryImageConfig reads a configured registry with the Bearer token its tokenCommand prints, run once, and gives labels, env and diff ids; an unconfigured host is not read, GHCR is read with its anonymous pull token, and a failed read names no token", async () => {
  const log = path.join(scratch, "token-runs.log");
  const registries = [
    {
      host: "registry.example.test",
      tokenCommand: ["/bin/sh", "-c", `echo run >> '${log}'; printf '  ${TOKEN}\\n'`],
    },
  ];
  const root = "https://registry.example.test/v2/acme/deploy/worker";
  const image = `registry.example.test/acme/deploy/worker@${INDEX}`;
  const { retries } = retryRule();
  const fetched = standinFetch(imageRoutes(root));
  expect(await registryImageConfig(image, { registries, fetch: fetched.send, retries })).toEqual({
    labels: { "org.opencontainers.image.revision": "de".repeat(20) },
    env: ["OMP_PROFILE=legion"],
    diffIds: LAYERS,
  });
  expect(fetched.reads).toEqual(
    [`${root}/manifests/${INDEX}`, `${root}/manifests/${AMD64}`, `${root}/blobs/${CONFIG}`].map(
      (url) => ({ url, authorization: `Bearer ${TOKEN}` })
    )
  );

  // A host the configuration does not name, and that is not GHCR, is not read.
  const unnamed = standinFetch(imageRoutes("https://other.example.test/v2/acme/worker"));
  expect(
    await registryImageConfig(`other.example.test/acme/worker@${INDEX}`, {
      registries,
      fetch: unnamed.send,
      retries,
    })
  ).toBeUndefined();
  expect(unnamed.reads).toEqual([]);

  // GHCR, which the configuration does not name, is read with the anonymous pull token it gives.
  const ghcrRoot = "https://ghcr.io/v2/acme/worker";
  const tokenUrl = "https://ghcr.io/token?scope=repository%3Aacme%2Fworker%3Apull";
  const ghcr = standinFetch({ [tokenUrl]: { token: "anonymous" }, ...imageRoutes(ghcrRoot) });
  expect(
    (
      await registryImageConfig(`ghcr.io/acme/worker@${INDEX}`, {
        registries,
        fetch: ghcr.send,
        retries,
      })
    )?.diffIds
  ).toEqual(LAYERS);
  expect(ghcr.reads[0]).toEqual({ url: tokenUrl, authorization: undefined });
  expect(ghcr.reads.slice(1).map((read) => read.authorization)).toEqual(
    Array(3).fill("Bearer anonymous")
  );

  // A registry that fails on every try: the error names the image and the last try's status, and
  // no token.
  const failing = standinFetch(imageRoutes(root), (url) =>
    url === `${root}/manifests/${INDEX}` ? 500 : undefined
  );
  let said = "";
  try {
    await registryImageConfig(image, { registries, fetch: failing.send, retries });
  } catch (error) {
    said = (error as Error).message;
  }
  expect(said).toBe(`reading ${image}: registry HTTP 500 on the last of 5 tries`);
  // The token command ran once for both reads of the configured registry.
  expect(readFileSync(log, "utf8")).toBe("run\n");
});

// Catches: a token command that never ends holding the read of its registry, and every later
// read of that registry in the process (its promise is kept), for ever.
test("registryImageConfig ends a token command that runs past its time limit, and its failure names the registry's host alone, not the command or what it printed", async () => {
  const registries = [
    {
      host: "slow.example.test",
      tokenCommand: ["/bin/sh", "-c", "printf tok-made-up-partial; exec sleep 5"],
    },
  ];
  const fetched = standinFetch(imageRoutes("https://slow.example.test/v2/acme/worker"));
  const started = performance.now();
  const said = await registryImageConfig(`slow.example.test/acme/worker@${INDEX}`, {
    registries,
    fetch: fetched.send,
    retries: retryRule().retries,
    tokenTimeoutMs: 200,
  }).then(
    () => "read",
    (error: Error) => error.message
  );
  expect(said).toBe(
    "`the token command of slow.example.test` ran past its limit of 200 ms, so it was ended"
  );
  expect(performance.now() - started).toBeLessThan(4_000);
  expect(fetched.reads).toEqual([]);
});

// registryImageConfig on a configured registry under load: its token command prints a random
// fixture token, and each request's time limit is short so a row whose request meets it takes
// well under a second.
const LOADED_TOKEN = fixtureSecret("tok");
const LOADED = [{ host: "loaded.example.test", tokenCommand: ["printf", LOADED_TOKEN] }];
const LOADED_ROOT = "https://loaded.example.test/v2/acme/worker";
const LOADED_IMAGE = `loaded.example.test/acme/worker@${INDEX}`;
const REQUEST_LIMIT_MS = 50;
/** The line the rule says before it sends `url` again after `failure`, at its `retry`-th retry. */
const sentAgain = (url: string, wait: number, retry: number, failure: string) =>
  `build-cases: the registry read of ${url} failed (${failure}); sending it again in ${wait} s, retry ${retry} of 4`;

// Catches: one time limit for the whole read, and each request sent once, so a read slowed by load
// on the pod (23.6 s for one image, measured) failed its profile with "The operation timed out.".
test("registryImageConfig gives each request its own time limit and sends a request that timed out or answered 503 again by the runner's retry rule, counted and said, and reads the config", async () => {
  const rule = retryRule();
  const fetched = standinFetch(imageRoutes(LOADED_ROOT), (url, n) => {
    if (n > 0) return undefined;
    if (url === `${LOADED_ROOT}/manifests/${INDEX}`) return "hang";
    return url === `${LOADED_ROOT}/blobs/${CONFIG}` ? 503 : undefined;
  });
  expect(
    await registryImageConfig(LOADED_IMAGE, {
      registries: LOADED,
      fetch: fetched.send,
      retries: rule.retries,
      requestTimeoutMs: REQUEST_LIMIT_MS,
    })
  ).toEqual({
    labels: { "org.opencontainers.image.revision": "de".repeat(20) },
    env: ["OMP_PROFILE=legion"],
    diffIds: LAYERS,
  });
  const [index, amd64, blob] = [`manifests/${INDEX}`, `manifests/${AMD64}`, `blobs/${CONFIG}`].map(
    (each) => `${LOADED_ROOT}/${each}`
  );
  expect(fetched.reads.map((read) => read.url)).toEqual([index, index, amd64, blob, blob]);
  expect(rule.retries.record()).toEqual({ "HTTP 503": 1, "time limit": 1 });
  expect(rule.waits).toEqual([5_000, 5_000]);
  expect(rule.lines).toEqual([
    sentAgain(index ?? "", 5, 1, `timed out after ${REQUEST_LIMIT_MS} ms`),
    sentAgain(blob ?? "", 5, 1, "HTTP 503"),
  ]);
});

// Catches: a registry's "too many requests" failing the read at its first answer, as a refusal
// for good does.
test("registryImageConfig sends a request that answers 429 on every try again by the rule, and after its last try fails naming the image and the last failure, never the token", async () => {
  const rule = retryRule();
  const index = `${LOADED_ROOT}/manifests/${INDEX}`;
  const fetched = standinFetch(imageRoutes(LOADED_ROOT), (url) =>
    url === index ? 429 : undefined
  );
  const said = await registryImageConfig(LOADED_IMAGE, {
    registries: LOADED,
    fetch: fetched.send,
    retries: rule.retries,
    requestTimeoutMs: REQUEST_LIMIT_MS,
  }).then(
    () => "read",
    (error: Error) => error.message
  );
  expect(said).toBe(
    `reading ${LOADED_IMAGE}: registry HTTP 429 (too many requests) on the last of 5 tries`
  );
  expect(fetched.reads.map((read) => read.url)).toEqual(Array(5).fill(index));
  expect(rule.retries.record()).toEqual({ "HTTP 429": 4 });
  expect(rule.waits).toEqual([5_000, 15_000, 45_000, 135_000]);
  expect([said, ...rule.lines].filter((line) => line.includes(LOADED_TOKEN))).toEqual([]);
});

// Catches: a configured registry that refuses its token read as an image with no OCI config, so
// the case was left out as if the image had no labels.
test("registryImageConfig of a configured registry that answers 403 or 401 fails at once naming the host, the image and the status, and a 404 is no such image", async () => {
  for (const status of [403, 401]) {
    const rule = retryRule();
    const fetched = standinFetch(imageRoutes(LOADED_ROOT), () => status);
    const said = await registryImageConfig(LOADED_IMAGE, {
      registries: LOADED,
      fetch: fetched.send,
      retries: rule.retries,
    }).then(
      () => "read",
      (error: Error) => error.message
    );
    expect(said).toBe(
      `reading ${LOADED_IMAGE}: loaded.example.test refused the token of its tokenCommand (HTTP ${status})`
    );
    expect(fetched.reads).toHaveLength(1);
    expect(rule.waits).toEqual([]);
  }
  const rule = retryRule();
  const missing = standinFetch(imageRoutes(LOADED_ROOT), () => 404);
  expect(
    await registryImageConfig(LOADED_IMAGE, {
      registries: LOADED,
      fetch: missing.send,
      retries: rule.retries,
    })
  ).toBeUndefined();
  expect(missing.reads).toHaveLength(1);
  expect(rule.waits).toEqual([]);
});

// Catches: GHCR's anonymous read of an image that is not public (its token or its manifest
// answered 401) taken for a refusal of a token, as a configured registry's is.
test("registryImageConfig of GHCR's anonymous read that answers 401, for its token or its manifest, is no public image: undefined, not sent again", async () => {
  const root = "https://ghcr.io/v2/acme/worker";
  const tokenUrl = "https://ghcr.io/token?scope=repository%3Aacme%2Fworker%3Apull";
  for (const refused of [tokenUrl, `${root}/manifests/${INDEX}`]) {
    const rule = retryRule();
    const fetched = standinFetch(
      { [tokenUrl]: { token: "anonymous" }, ...imageRoutes(root) },
      (url) => (url === refused ? 401 : undefined)
    );
    expect(
      await registryImageConfig(`ghcr.io/acme/worker@${INDEX}`, {
        fetch: fetched.send,
        retries: rule.retries,
      })
    ).toBeUndefined();
    expect(fetched.reads.map((read) => read.url).at(-1)).toBe(refused);
    expect(rule.waits).toEqual([]);
  }
});

// sessionPodImage on made-up repositories: acme/deploy pins the pod's image in pods/stack.yaml
// and builds it from pods/Dockerfile on a base that acme/worker's worker.Dockerfile builds.
const DEPLOY_COMMIT = "1".repeat(40);
const BUILD = "2".repeat(40);
const WORKER_COMMIT = "3".repeat(40);
const POD_AT = "2026-11-20T00:00:00Z";
const POD = {
  repository: "acme/deploy",
  pinFile: "pods/stack.yaml",
  pinKey: "pods:image",
  dockerfile: "pods/Dockerfile",
  workerRepository: "acme/worker",
  workerDockerfile: "worker.Dockerfile",
};
/** The pin file pods/stack.yaml of a pin line `line` (`pods:image: <pinned>`, or another spelling
 *  of the key), under a comment that names BUILD. */
const pinText = (line: string) => `config:\n  # The job built commit ${BUILD}.\n  ${line}\n`;

/** The GitHub and registry reads sessionPodImage makes: acme/deploy's branch main at
 *  DEPLOY_COMMIT holds `pin`; its pods/Dockerfile at BUILD builds on `base` and links
 *  /opt/pod/pi-pod into the profile `pod`; acme/worker's worker.Dockerfile at WORKER_COMMIT links
 *  /opt/worker/pi-worker into it; `base`'s config names WORKER_COMMIT and holds the first two of
 *  `pinned`'s diff ids; the build provenance of a digest names `attested`, or no commit. */
function podReads(pin: string, base: string, pinned: string, attested?: string) {
  const files: Record<string, string> = {
    [`acme/deploy@${DEPLOY_COMMIT}:pods/stack.yaml`]: pin,
    [`acme/deploy@${BUILD}:pods/Dockerfile`]: `ARG BASE=${base}\nFROM \${BASE}\nRUN omp plugin link /opt/pod/pi-pod\n`,
    [`acme/worker@${WORKER_COMMIT}:worker.Dockerfile`]:
      "FROM scratch\nENV OMP_PROFILE=pod\nRUN omp plugin link /opt/worker/pi-worker\n",
  };
  const configs: Record<string, ImageConfig> = {
    [base]: {
      labels: { "org.opencontainers.image.revision": WORKER_COMMIT },
      env: ["OMP_PROFILE=pod"],
      diffIds: LAYERS.slice(0, 2),
    },
    [pinned]: { labels: {}, env: [], diffIds: LAYERS },
  };
  return {
    githubBranch: async () => ({ branch: "main", from: "the stand-in" }),
    commitAt: async () => DEPLOY_COMMIT,
    text: async (repo: string, commit: string, file: string) => files[`${repo}@${commit}:${file}`],
    attestedCommit: async () =>
      attested === undefined ? undefined : { commit: attested, uri: "git+https://example.test" },
    imageConfig: async (image: string) => configs[image],
  };
}

// Catches: a pinned image or a base on a registry host with a port (`host:5000/…`, which the
// configuration's registries take) refused as having no pinned digest.
test("sessionPodImage reads a pinned image and a base whose registry host has a port", async () => {
  const pinned = `registry.example.test:5000/acme/pod@sha256:${"a".repeat(64)}`;
  const base = `registry.example.test:5000/acme/worker@sha256:${"b".repeat(64)}`;
  const pod = await sessionPodImage(
    podReads(pinText(`pods:image: ${pinned}`), base, pinned),
    POD,
    "pod",
    POD_AT
  );
  expect({ repo: pod.repo, sha: pod.sha, directories: pod.directories }).toEqual({
    repo: "acme/worker",
    sha: WORKER_COMMIT,
    directories: ["/opt/worker/pi-worker", "/opt/pod/pi-pod"],
  });
  expect(pod.why).toStartWith(
    `Inference: pods/stack.yaml at ${DEPLOY_COMMIT}:3 selects the image in force at prompt build ${POD_AT}.`
  );
});

// Catches: a pin whose key YAML reads but no line starts with (a quoted key) cited as line 0,
// with an empty comment read for its build commit.
test("sessionPodImage of a pin file with no line that starts with its key says so, where it refuses the pin and where build provenance names the commit", async () => {
  const pinned = `registry.example.test/acme/pod@sha256:${"a".repeat(64)}`;
  const base = `registry.example.test/acme/worker@sha256:${"b".repeat(64)}`;
  const quoted = pinText(`"pods:image": ${pinned}`);
  const where = `pods/stack.yaml at ${DEPLOY_COMMIT} (no line of it starts with pods:image:)`;
  const refused = await sessionPodImage(podReads(quoted, base, pinned), POD, "pod", POD_AT).then(
    () => "read",
    (error: Error) => error.message
  );
  expect(refused).toBe(
    `pod profile not measurable: ${where} names no build commit for sha256:${"a".repeat(64)}`
  );
  const attested = await sessionPodImage(podReads(quoted, base, pinned, BUILD), POD, "pod", POD_AT);
  expect(attested.sha).toBe(WORKER_COMMIT);
  expect(attested.why).toStartWith(
    `Inference: ${where} selects the image in force at prompt build ${POD_AT}. Build provenance names ${BUILD}.`
  );
});
