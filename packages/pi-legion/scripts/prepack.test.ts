import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import * as path from "node:path";
import { daemonContract, type PluginManifest } from "@legion/contracts/daemon-contract";
import { stageSkills } from "@legion/pi-shared/test/skills-guard";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

const PACKAGE_ROOT = path.resolve(import.meta.dir, "..");
const REPO_ROOT = path.resolve(PACKAGE_ROOT, "../..");
const manifest: {
  readonly name: string;
  readonly omp?: {
    readonly extensions?: readonly string[];
    readonly skills?: readonly string[];
  };
  readonly scripts?: Record<string, string>;
} = JSON.parse(readFileSync(path.join(PACKAGE_ROOT, "package.json"), "utf8"));

describe("omp.skills / prepack manifest contract", () => {
  test("package.json declares omp.skills alongside omp.extensions, both pointing into dist/", () => {
    // omp discovers plugin skills only at `<plugin root>/skills` or at directories the
    // manifest's `omp.skills` array names. This package ships skills at `dist/skills` (staged
    // there by the prepack, not the package root), so the manifest must name it explicitly —
    // dropping this field silently regresses every installed session back to no Legion skills.
    expect(manifest.omp?.skills).toEqual(["dist/skills"]);
    // The release workflow rewrites `omp.extensions` (only) to the packed bundle path right
    // before packing; `omp.skills` ships as committed, so it must already be the `dist/` path a
    // packed tarball actually contains, in both source and released manifests.
    expect(manifest.omp?.extensions?.every((entry) => entry.startsWith("extensions/"))).toBe(true);
  });

  test("prepack is the shared script, and postpack removes the directory it stages", () => {
    // The one script packs both plugins and holds the skills partition; `postpack` removes the
    // directory it staged so a dev checkout's `dist/` doesn't retain packed-only content after a
    // local `bun pm pack`.
    expect(manifest.scripts?.prepack).toBe("../../scripts/pi-plugin-prepack.sh");
    expect(manifest.scripts?.postpack).toBe("rm -rf dist/skills");
  });

  describe("the staged skills", () => {
    // Staged in a hook: Bun runs this body where the rows do not run, and a staging here would
    // outlive the run.
    let staged = "";
    beforeAll(() => {
      staged = stageSkills(manifest.name);
    });
    afterAll(() => {
      if (staged !== "") rmSync(path.dirname(staged), { recursive: true, force: true });
    });

    test("are exactly this plugin's partition: the skills a Legion pane's prompts load", () => {
      expect(readdirSync(staged).sort()).toEqual([
        "legion-architect",
        "legion-controller",
        "legion-oracle",
        "legion-retro",
        "legion-worker",
        "simplify-settled-diff",
      ]);
    });
  });
});

describe("the e2e pack (scripts/e2e/lib/pack-plugin.sh)", () => {
  const work = temporaryDirectory("pi-legion-pack");
  // Catches: a pack that writes this checkout's manifest, even for the seconds it runs. A test of
  // this checkout that reads the committed manifest while a pack runs beside it (a skill-scenario
  // rig row builds a profile from this checkout under `bun test --parallel`) read the rewrite:
  // src/daemon-api-version.test.ts got `legion: { daemonApiVersion: <this tree's contract> }` and
  // took two merge groups out of the queue. Every write moves the file's change time, a rewrite that
  // puts the bytes back included, so the row fails such a pack too.
  test("packs from a copy: the checkout's manifest keeps its bytes and its change time, and the packed one declares this tree's contract", () => {
    const file = path.join(PACKAGE_ROOT, "package.json");
    const before = { bytes: readFileSync(file, "utf8"), ctime: statSync(file).ctimeMs };
    const out = path.join(work, "out");
    const pack = Bun.spawnSync(
      ["bash", path.join(REPO_ROOT, "scripts/e2e/lib/pack-plugin.sh"), "pi-legion", out],
      { stderr: "pipe" }
    );
    expect({
      exit: pack.exitCode,
      stderr: pack.exitCode === 0 ? "" : pack.stderr.toString(),
    }).toEqual({ exit: 0, stderr: "" });
    const tarball = pack.stdout.toString().trim();
    const packed = JSON.parse(
      Bun.spawnSync(["tar", "xzOf", tarball, "package/package.json"]).stdout.toString()
    ) as PluginManifest;
    expect(packed.legion?.daemonApiVersion).toBe(
      daemonContract(JSON.parse(before.bytes) as PluginManifest, import.meta.dir)
    );
    expect({ bytes: readFileSync(file, "utf8"), ctime: statSync(file).ctimeMs }).toEqual(before);
  }, 120_000);
});
