import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { type RegistryFetch, registryImageConfig } from "./pod-image";

// registryImageConfig on made-up registries: a stand-in fetch answers each read, and nothing
// reaches a network. Every host, image and token here is made up.

const scratch = temporaryDirectory("skill-cuts-pod-image", { real: true });

const INDEX = `sha256:${"a".repeat(64)}`;
const AMD64 = `sha256:${"d".repeat(64)}`;
const ARM64 = `sha256:${"e".repeat(64)}`;
const CONFIG = `sha256:${"c".repeat(64)}`;
const TOKEN = "tok-made-up-1";
const LAYERS = [1, 2, 3].map((n) => `sha256:${String(n).repeat(64)}`);

/** A stand-in fetch over `routes` (the JSON body of each URL; a 500 for `fail`, a 404 for any
 *  other URL), and the URL and Authorization header of each read it took. */
function standinFetch(routes: Readonly<Record<string, unknown>>, fail?: string) {
  const reads: { url: string; authorization: string | undefined }[] = [];
  const send: RegistryFetch = async (url, init) => {
    reads.push({ url, authorization: init.headers?.Authorization });
    if (url === fail) return new Response("upstream failed", { status: 500 });
    if (!Object.hasOwn(routes, url)) return new Response("not found", { status: 404 });
    return Response.json(routes[url]);
  };
  return { reads, send };
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
test("registryImageConfig reads a configured registry with the Bearer token its tokenCommand prints, run once, and gives labels, env and diff ids; an unconfigured host is not read, GHCR is read as before, and a failed read names no token", async () => {
  const log = path.join(scratch, "token-runs.log");
  const registries = [
    {
      host: "registry.example.test",
      tokenCommand: ["/bin/sh", "-c", `echo run >> '${log}'; printf '  ${TOKEN}\\n'`],
    },
  ];
  const root = "https://registry.example.test/v2/acme/deploy/worker";
  const image = `registry.example.test/acme/deploy/worker@${INDEX}`;
  const fetched = standinFetch(imageRoutes(root));
  expect(await registryImageConfig(image, { registries, fetch: fetched.send })).toEqual({
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
    })
  ).toBeUndefined();
  expect(unnamed.reads).toEqual([]);

  // GHCR is read with the anonymous pull token it gives, as before.
  const ghcrRoot = "https://ghcr.io/v2/acme/worker";
  const tokenUrl = "https://ghcr.io/token?scope=repository%3Aacme%2Fworker%3Apull";
  const ghcr = standinFetch({ [tokenUrl]: { token: "anonymous" }, ...imageRoutes(ghcrRoot) });
  expect(
    (await registryImageConfig(`ghcr.io/acme/worker@${INDEX}`, { registries, fetch: ghcr.send }))
      ?.diffIds
  ).toEqual(LAYERS);
  expect(ghcr.reads[0]).toEqual({ url: tokenUrl, authorization: undefined });
  expect(ghcr.reads.slice(1).map((read) => read.authorization)).toEqual(
    Array(3).fill("Bearer anonymous")
  );

  // A registry that fails: the error names the image and the status, and no token.
  const failing = standinFetch(imageRoutes(root), `${root}/manifests/${INDEX}`);
  let said = "";
  try {
    await registryImageConfig(image, { registries, fetch: failing.send });
  } catch (error) {
    said = (error as Error).message;
  }
  expect(said).toBe(`reading ${image}: registry HTTP 500`);
  expect(said).not.toContain(TOKEN);
  // The token command ran once for both reads of the configured registry.
  expect(readFileSync(log, "utf8")).toBe("run\n");
});
