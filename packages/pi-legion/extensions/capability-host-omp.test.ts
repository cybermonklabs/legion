import { afterEach, expect, test } from "bun:test";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import {
  type Cleanup,
  messageStream,
  ompRoot,
  serveStandin,
  spawnRpc,
  writeStandinProfile,
} from "@legion/pi-shared/test/omp-harness";
import { until } from "@legion/test-fixtures/wait";
import { CAPABILITY_PROBE_OUT, COST_CAP_PROBE_OUT } from "../test/capability-host-probe";

// What pi-legion reads of Oh My Pi's own modules, on the real Oh My Pi. Those modules resolve
// only inside the binary's bundle, so the unit tests fake them and the typecheck reads only
// `omp-host.d.ts`: neither sees what the host's functions really answer. Each case runs the reader
// inside the binary (test/capability-host-probe.ts) and reads what it found.
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

/** Starts the binary with the probe loaded, after `prepare` has written into the session's
 *  workspace, and answers the report the probe wrote. */
async function probeReport(prepare: (workspace: string) => Promise<void>): Promise<unknown> {
  if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
  const { root, home, workspace, sessions } = await ompRoot(omp, "capability-host-omp-", cleanup);
  await prepare(workspace);
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
  await rpc.end();
  return JSON.parse(written);
}

// The capability report's production host (`ompCapabilityHost` in src/capability-report.ts). Oh
// My Pi's `discoverExtensionPaths` returned bare paths at v18.18.6-sami.20261123-024918 and
// returns `{path, scope}` entries at v18.18.6-sami.20261126-222037
// (`extensibility/extensions/loader.ts:804-809`), and an adapter that passed the entries on
// failed the repository-extensions row of every repository that carries an extension or a skill.
// This case runs against a workspace that carries one of each. Measured on 2026-11-25 against
// v18.18.6-sami.20261124-152326, which returns entries too: the adapter that passed them on fails
// this case, the row reading `The "paths[0]" property must be of type string, got object`, and the
// one that takes each entry's path passes it.
test.skipIf(omp === undefined && !onActions)(
  "the production host hands the repository-extensions row each extension's path, and the row finds the repository's own extension and skill",
  async () => {
    let extension = "";
    const report = await probeReport(async (workspace) => {
      extension = path.join(workspace, ".omp", "extensions", "repository-probe.ts");
      await mkdir(path.dirname(extension), { recursive: true });
      await writeFile(extension, "export default function repositoryProbe(): void {}\n");
      const skill = path.join(workspace, ".omp", "skills", "repository-probe", "SKILL.md");
      await mkdir(path.dirname(skill), { recursive: true });
      await writeFile(
        skill,
        "---\nname: repository-probe\ndescription: A skill the repository carries.\n---\n\nNothing to do.\n"
      );
    });

    expect(report).toMatchObject({
      capabilities: {
        // Each a path, as the checks read it, never the host's `{path, scope}` entry.
        extensionPaths: expect.arrayContaining([extension]),
        row: {
          name: "repository-extensions",
          ok: true,
          detail:
            "discovered: .omp/extensions/repository-probe.ts, .omp/skills/repository-probe/SKILL.md",
        },
      },
    });
  }
);

// The host's cyber-policy classifier, which extensions/legion.ts reads from
// `@oh-my-pi/pi-ai/error` for the turn-outcome reporter: pi-legion's one runtime import of a
// pi-ai subpath. The reporter turns a failure to read it into a warning and a report with no
// `code` (src/turn-outcome.ts), and turn-outcome.test.ts injects a classifier of its own, so only
// this case sees the import resolve in the binary and the host class a refusal by its words.
test.skipIf(omp === undefined && !onActions)(
  "the host's own classifier, read as the turn-outcome reporter reads it, classes a cyber-policy refusal as one and another refusal of the same status as none",
  async () => {
    const report = await probeReport(async () => undefined);

    expect(report).toMatchObject({ cyberPolicyRefusal: { refusal: true, other: false } });
  }
);

test.skipIf(omp === undefined && !onActions)(
  "the pinned host exposes a cost-cap drop before any turn starts",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const { root, home, workspace, sessions } = await ompRoot(omp, "cost-cap-host-omp-", cleanup);
    const { base, requests } = serveStandin(cleanup, (url) => {
      if (url.pathname !== "/anthropic/v1/messages") return Response.json({ data: [] });
      return new Response(messageStream([{ type: "text", text: "Done." }], "msg_cost_cap"), {
        headers: { "content-type": "text/event-stream" },
      });
    });
    await writeStandinProfile(home, base, ["session:", "  maxCost: 1"]);
    // The one reply uses five input tokens. Use $1 per input token so it reaches the cap.
    const models = path.join(home, ".omp", "agent", "models.yml");
    const profile = await readFile(models, "utf8");
    await writeFile(models, profile.replace("cost: {input: 0,", "cost: {input: 1000000,"));

    const out = path.join(root, "cost-cap-probe.json");
    let starts = 0;
    const warmup = Promise.withResolvers<void>();
    const rpc = spawnRpc(
      omp,
      {
        extensions: [path.join(import.meta.dir, "..", "test", "capability-host-probe.ts")],
        home,
        workspace,
        sessions,
        env: { [COST_CAP_PROBE_OUT]: out },
        onFrame: (frame) => {
          if (!("type" in frame)) return;
          if (frame.type === "agent_start") starts += 1;
          if (frame.type === "prompt_result" && "sessionSettled" in frame && frame.sessionSettled) {
            warmup.resolve();
          }
          if (frame.type === "session_settled") warmup.resolve();
        },
      },
      cleanup
    );
    rpc.send({ type: "prompt", message: "Spend five dollars in the stand-in session." });
    await Promise.race([warmup.promise, rpc.closed]);
    rpc.send({ type: "prompt", message: "/probe-cost-cap" });
    const written = await Promise.race([
      until("the probe's dropped-send report", () => readFile(out, "utf8").catch(() => undefined)),
      rpc.closed,
    ]);
    await rpc.end();

    expect(JSON.parse(written)).toEqual({
      dropped: true,
      message:
        "Cost cap reached: this session spent $5.00 of its $1.00 cap (--max-cost / session.maxCost), so the next model request was not sent.",
    });
    expect(starts).toBe(1);
    expect(requests.filter(({ path }) => path === "/anthropic/v1/messages")).toHaveLength(1);
  }
);
