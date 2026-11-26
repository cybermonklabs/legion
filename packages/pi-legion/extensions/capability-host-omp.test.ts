import { afterEach, expect, test } from "bun:test";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import {
  type Cleanup,
  ompRoot,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { until } from "@legion/test-fixtures/wait";
import { CAPABILITY_PROBE_OUT } from "../test/capability-host-probe";

// The capability report's production host (`ompCapabilityHost` in src/capability-report.ts) on
// the real Oh My Pi. Its members import the host's own modules, which resolve only inside the
// binary's bundle, so `capability-report.test.ts` fakes the whole host and the typecheck reads
// only `omp-host.d.ts`: neither sees what the host's functions really return. Oh My Pi's
// `discoverExtensionPaths` returned bare paths at v18.18.6-sami.20261123-024918 and returns
// `{path, scope}` entries at v18.18.6-sami.20261125-164517
// (`extensibility/extensions/loader.ts:804-809`), and an adapter that passed the entries on
// failed the repository-extensions row of every repository that carries an extension or a skill.
// This case runs the adapter inside the binary (test/capability-host-probe.ts) against a
// workspace that carries one of each, and reads what the row found. Measured on 2026-11-25
// against v18.18.6-sami.20261124-152326, which returns entries too: the adapter that passed them
// on fails this case, the row reading `The "paths[0]" property must be of type string, got
// object`, and the one that takes each entry's path passes it.
// LEGION_TEST_OMP names the binary: the fork pin in the repository's .omp-pin, which CI's
// pi-legion job installs (.github/actions/install-omp). The probe needs no NATS server, so a run
// without the binary skips, except on GitHub Actions, where a skip would hide the only run of the
// check on the host that ships it.
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

test.skipIf(omp === undefined && !onActions)(
  "the production host hands the repository-extensions row each extension's path, and the row finds the repository's own extension and skill",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const { root, home, workspace, sessions } = await ompRoot(omp, "capability-host-omp-", cleanup);
    const extension = path.join(workspace, ".omp", "extensions", "repository-probe.ts");
    await mkdir(path.dirname(extension), { recursive: true });
    await writeFile(extension, "export default function repositoryProbe(): void {}\n");
    const skill = path.join(workspace, ".omp", "skills", "repository-probe", "SKILL.md");
    await mkdir(path.dirname(skill), { recursive: true });
    await writeFile(
      skill,
      "---\nname: repository-probe\ndescription: A skill the repository carries.\n---\n\nNothing to do.\n"
    );
    // No turn runs; the profile still needs a model, and none may reach a real one.
    const { base } = serveStandin(cleanup, () => new Response("not served", { status: 404 }));
    await writeStandinProfile(home, base);

    const out = path.join(root, "capability-probe.json");
    const rpc = spawnRpc(
      omp,
      {
        extensions: [path.join(import.meta.dir, "..", "test", "capability-host-probe.ts")],
        home,
        workspace,
        sessions,
        env: { [CAPABILITY_PROBE_OUT]: out },
        onFrame: () => undefined,
      },
      cleanup
    );
    const written = await Promise.race([
      until("the probe's report", () => readFile(out, "utf8").catch(() => undefined)),
      rpc.closed,
    ]);
    const report: unknown = JSON.parse(written);

    expect(report).toMatchObject({
      // Each a path, as the checks read it, never the host's `{path, scope}` entry.
      extensionPaths: expect.arrayContaining([extension]),
      row: {
        name: "repository-extensions",
        ok: true,
        detail:
          "discovered: .omp/extensions/repository-probe.ts, .omp/skills/repository-probe/SKILL.md",
      },
    });
    await rpc.end();
  }
);
