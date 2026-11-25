import { expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { loadConfig, parseConfig, SkillCutsConfig, storeNamed } from "./config";

// The one loader of `skill-cuts.json` (config.ts): every key is checked, and a missing or unknown
// key is refused by name. Every value here is made up; testdata/F7-skill-cuts.json is the made-up
// configuration F7's capture used.

/** A configuration with every required key, each a made-up value; it leaves out the optional
 *  keys, devboxClaudeConfigDir, devboxHome, clones, sessionPod, ompPin, registries and
 *  deploymentImages. */
function valid(): Record<string, unknown> {
  return {
    owners: ["acme", "acme-labs"],
    stores: [
      { name: "store_a", context: "devbox" },
      { name: "store_b", context: "legion:ACME" },
    ],
    psqlCommand: "/opt/acme/bin/read-store",
    gatewayBaseUrl: "http://127.0.0.1:4000",
    servedModels: ["anthropic/claude-alpha", "openai/gpt-alpha"],
    excludePaths: [{ repo: "acme/skills", prefix: "pipeline/candidates/" }],
    targets: ["acme/skills", "acme-labs/tools"],
    cuts: [{ id: "skills-1", repo: "acme/skills", pr: 1 }],
    windowStart: "2026-10-23T00:00:00Z",
    windowEnd: "2026-11-20T00:00:00Z",
  };
}

/** The refusal `parseConfig` gives for `value`, or a failure when it gives none. */
function refusal(value: unknown): string {
  try {
    parseConfig(value);
  } catch (error) {
    return (error as Error).message;
  }
  throw new Error("the configuration was not refused");
}

test("a configuration with every required key loads", () => {
  const config = parseConfig(valid());
  expect(config.stores.map((store) => store.name)).toEqual(["store_a", "store_b"]);
  expect(config.windowEnd).toBe("2026-11-20T00:00:00Z");
});

test.each(
  Object.keys(SkillCutsConfig.shape).filter(
    (key) =>
      key !== "devboxClaudeConfigDir" &&
      key !== "devboxHome" &&
      key !== "clones" &&
      key !== "sessionPod" &&
      key !== "ompPin" &&
      key !== "registries" &&
      key !== "deploymentImages"
  )
)("a configuration without %s is refused by name", (key) => {
  const value = valid();
  delete value[key];
  expect(refusal(value)).toContain(`${key}: missing`);
});

test("sessionPod accepts a deployment pin and two repository-relative Dockerfiles", () => {
  const sessionPod = {
    repository: "acme/deploy",
    pinFile: "pods/stack.yaml",
    pinKey: "pods:image",
    dockerfile: "pods/Dockerfile",
    workerRepository: "acme/worker",
    workerDockerfile: "docker/worker.Dockerfile",
  };
  expect(parseConfig(valid()).sessionPod).toBeUndefined();
  expect(parseConfig({ ...valid(), sessionPod }).sessionPod).toEqual(sessionPod);
  expect(
    refusal({ ...valid(), sessionPod: { ...sessionPod, dockerfile: "../Dockerfile" } })
  ).toContain(
    "sessionPod.dockerfile: a file of the repository as a path in the repository, such as mise.toml"
  );
});

// Catches: a registry the runner reads with a token, or an image a deployment builds, stated in a
// shape the readers cannot use (a host with a path, a token command of no program, an image
// reference with its tag or digest, a Dockerfile outside the repository) loaded as if it could.
test("registries and deploymentImages are optional; a host with its token command and an image repository with its Dockerfile load, and any other is refused by name", () => {
  const registry = { host: "registry.example.test", tokenCommand: ["acme-token", "print"] };
  const deployed = {
    repository: "registry.example.test/acme/deploy/worker",
    dockerfile: "images/worker.Dockerfile",
  };
  expect(parseConfig(valid()).registries).toBeUndefined();
  expect(parseConfig(valid()).deploymentImages).toBeUndefined();
  const config = parseConfig({ ...valid(), registries: [registry], deploymentImages: [deployed] });
  expect(config.registries).toEqual([registry]);
  expect(config.deploymentImages).toEqual([deployed]);
  expect(
    parseConfig({ ...valid(), registries: [{ ...registry, host: "registry.example.test:5000" }] })
      .registries
  ).toHaveLength(1);
  expect(
    refusal({ ...valid(), registries: [{ ...registry, host: "registry.example.test/v2" }] })
  ).toContain("registries[0].host: a registry host, such as registry.example.test");
  expect(refusal({ ...valid(), registries: [{ ...registry, tokenCommand: [] }] })).toContain(
    "registries[0].tokenCommand:"
  );
  for (const repository of [
    `${deployed.repository}:latest`,
    `${deployed.repository}@sha256:${"a".repeat(64)}`,
    "worker",
  ]) {
    expect(refusal({ ...valid(), deploymentImages: [{ ...deployed, repository }] })).toContain(
      "deploymentImages[0].repository: an image reference without tag or digest, such as registry.example.test/acme/worker"
    );
  }
  expect(
    refusal({ ...valid(), deploymentImages: [{ ...deployed, dockerfile: "../Dockerfile" }] })
  ).toContain("deploymentImages[0].dockerfile:");
});

// Catches: a dev box's Oh My Pi pin the pool version rule cannot read (a file outside the
// repository, no key, a first pool-loading build that is no version) loaded as if it could.
test("ompPin is optional; a repository file, a key path and a first pool-loading version load, and any other is refused by name", () => {
  const ompPin = { file: "mise.toml", key: ["tools", "acme:omp"], firstPoolVersion: "18.16.0" };
  expect(parseConfig(valid()).ompPin).toBeUndefined();
  expect(parseConfig({ ...valid(), ompPin }).ompPin).toEqual(ompPin);
  const fork = { ...ompPin, firstPoolVersion: "18.16.0-fork.20261104-021732" };
  expect(parseConfig({ ...valid(), ompPin: fork }).ompPin).toEqual(fork);
  expect(refusal({ ...valid(), ompPin: { ...ompPin, file: "../mise.toml" } })).toContain(
    "ompPin.file: a file of the repository as a path in the repository, such as mise.toml"
  );
  expect(refusal({ ...valid(), ompPin: { ...ompPin, key: [] } })).toContain("ompPin.key:");
  expect(refusal({ ...valid(), ompPin: { ...ompPin, firstPoolVersion: "18.16" } })).toContain(
    "ompPin.firstPoolVersion: a version, such as 18.16.0 or 18.16.0-fork.1"
  );
  expect(refusal({ ...valid(), ompPin: { file: "mise.toml", key: ["tools"] } })).toContain(
    "ompPin.firstPoolVersion:"
  );
});

test("devboxClaudeConfigDir is optional, and an absolute path when given", () => {
  expect(parseConfig(valid()).devboxClaudeConfigDir).toBeUndefined();
  expect(
    parseConfig({ ...valid(), devboxClaudeConfigDir: "/srv/acme/dotfiles/.claude" })
      .devboxClaudeConfigDir
  ).toBe("/srv/acme/dotfiles/.claude");
  expect(refusal({ ...valid(), devboxClaudeConfigDir: "acme-dotfiles/.claude" })).toContain(
    "devboxClaudeConfigDir: an absolute path"
  );
});

// Catches: a dev box's home the context rule cannot match a [Skill file: …] header against (a
// relative path) loaded as if it could, or an older configuration without it refused.
test("devboxHome is optional, and an absolute path when given", () => {
  expect(parseConfig(valid()).devboxHome).toBeUndefined();
  expect(parseConfig({ ...valid(), devboxHome: "/home/dev" }).devboxHome).toBe("/home/dev");
  expect(refusal({ ...valid(), devboxHome: "home/dev" })).toContain("devboxHome: an absolute path");
});

// Catches: a pool clone made by hand (no installer records it) that the operator states for a
// repository the copy lister does not read, so it names no copy, or for a directory that is no
// path in the checkout.
test("clones is optional; a clone of a directory of a repository, both of the owners, loads, and any other is refused by name", () => {
  const clone = { repo: "acme/dotfiles", dir: ".private", clone: "acme-labs/private" };
  expect(parseConfig(valid()).clones).toBeUndefined();
  expect(parseConfig({ ...valid(), clones: [clone] }).clones).toEqual([clone]);
  // An owner's name is read in any case, as GitHub reads it.
  expect(
    parseConfig({ ...valid(), clones: [{ ...clone, clone: "Acme-Labs/private" }] }).clones
  ).toHaveLength(1);
  expect(refusal({ ...valid(), clones: [clone, { ...clone, clone: "other/private" }] })).toContain(
    "clones[1].clone: other/private is not a repository of an owner in owners"
  );
  expect(refusal({ ...valid(), clones: [{ ...clone, repo: "other/dotfiles" }] })).toContain(
    "clones[0].repo: other/dotfiles is not a repository of an owner in owners"
  );
  for (const dir of ["", "/home/alpha/.private", "~/.private", ".private/", "a/../b", "./x"]) {
    expect(refusal({ ...valid(), clones: [{ ...clone, dir }] })).toContain(
      "clones[0].dir: a directory of the checkout as a path in the repository, such as .secrets"
    );
  }
  expect(refusal({ ...valid(), clones: [{ ...clone, ref: "main" }] })).toContain(
    'clones[0]: unknown key "ref"'
  );
  expect(refusal({ ...valid(), clones: [{ repo: "acme/dotfiles", dir: ".private" }] })).toContain(
    "clones[0].clone: missing"
  );
});

test("an unknown key is refused by name", () => {
  expect(refusal({ ...valid(), gatewayKey: "/some/file" })).toContain(
    'the top level: unknown key "gatewayKey"'
  );
});

test("a store without its context, or with a key of its own, is refused by name", () => {
  const missing = valid();
  missing.stores = [{ name: "store_a" }];
  expect(refusal(missing)).toContain("stores[0].context: missing");
  const extra = valid();
  extra.stores = [{ name: "store_a", context: "devbox", host: "db.example" }];
  expect(refusal(extra)).toContain('stores[0]: unknown key "host"');
});

test.each([
  [
    "a store name that is no database name",
    { stores: [{ name: "Store-A", context: "devbox" }] },
    "stores[0].name",
  ],
  [
    "a context that is neither devbox nor legion:<PROJECT>",
    { stores: [{ name: "store_a", context: "pod" }] },
    "stores[0].context",
  ],
  [
    "a psqlCommand that is not absolute",
    { psqlCommand: "read-store" },
    "psqlCommand: an absolute path",
  ],
  ["a gateway that is no URL", { gatewayBaseUrl: "gateway:4000" }, "gatewayBaseUrl"],
  [
    "a served model that is not provider/model",
    { servedModels: ["claude-alpha"] },
    "servedModels[0]: a model as provider/model",
  ],
  [
    "a cut whose pr is not a number",
    { cuts: [{ id: "skills-1", repo: "acme/skills", pr: "1" }] },
    "cuts[0].pr",
  ],
  ["a target that is not owner/name", { targets: ["skills"] }, "targets[0]"],
  ["a window end that is no instant", { windowEnd: "2026-11-20" }, "windowEnd"],
  [
    "a window that ends before it starts",
    { windowEnd: "2026-10-01T00:00:00Z" },
    "windowEnd: windowEnd must be after windowStart",
  ],
  ["no store", { stores: [] }, "stores"],
])("%s is refused", (_name, change, fault) => {
  expect(refusal({ ...valid(), ...change })).toContain(fault);
});

test("loadConfig reads a file, and refuses one that is not JSON, naming the file", () => {
  const config = loadConfig(path.join(import.meta.dir, "testdata", "F7-skill-cuts.json"));
  expect(config.stores).toEqual([{ name: "store_a", context: "devbox" }]);
  const dir = mkdtempSync(path.join(process.env.TMPDIR ?? tmpdir(), "skill-cuts-config-"));
  try {
    const file = path.join(dir, "skill-cuts.json");
    writeFileSync(file, "{ owners: [] }");
    expect(() => loadConfig(file)).toThrow(`the configuration ${file} is not JSON`);
    writeFileSync(file, JSON.stringify({ ...valid(), owners: undefined }));
    expect(() => loadConfig(file)).toThrow("owners: missing");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("storeNamed finds a store the configuration names and refuses any other", () => {
  const config = parseConfig(valid());
  expect(storeNamed(config, "store_b").context).toBe("legion:ACME");
  expect(() => storeNamed(config, "store_c")).toThrow(
    'names no store "store_c"; it names store_a, store_b'
  );
});
