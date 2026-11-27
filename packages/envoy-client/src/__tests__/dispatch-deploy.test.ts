import { expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { keyedLayer } from "../../test/keyed-layer";
import { runDispatchCli } from "../dispatch-cli";

const directory = temporaryDirectories("dispatch-deploy");
const commit = "1111111111111111111111111111111111111111";
const deploy = {
  id: 7,
  repo: "acme/widgets",
  program: "worker",
  commit,
  deployed_at: "2026-11-20T00:00:00Z",
  source: "route",
  url: "https://example.com/deploy/7",
  run_attempt: null,
  recorded_at: "2026-11-20T00:00:01Z",
  recorded_by: { kind: "user", id: "alice" },
};
const record = ["deploy", "--repo", deploy.repo, "--program", deploy.program, "--commit", commit];

async function cli(argv: readonly string[]) {
  const root = directory();
  const requests: { method: string; path: string; query: string; body: unknown }[] = [];
  let output = "";
  const code = await runDispatchCli(
    argv,
    {
      HOME: root,
      DISPATCH_STATE_DIR: root,
      DISPATCH_URL: "https://dispatch.example.com",
      DISPATCH_TOKEN: fixtureSecret("dsp"),
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: "deploy-cli",
    },
    {
      cwd: root,
      stdout: (text) => {
        output += text;
      },
      stderr: () => {},
      readText: () => {
        throw new Error("this call reads no file");
      },
      // The deploy write is keyed, so the stand-in answers behind Dispatch's keyed layer, which
      // names the write's key back on each 2xx, as Dispatch's own answers do.
      fetchImpl: Object.assign(
        keyedLayer(
          async (input: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) => {
            const request = new Request(input, init);
            const url = new URL(request.url);
            requests.push({
              method: request.method,
              path: url.pathname,
              query: url.search,
              body: request.method === "POST" ? await request.json() : null,
            });
            if (url.pathname !== "/api/v1/deploys")
              return Response.json({ error: "unexpected route" }, { status: 500 });
            return request.method === "POST"
              ? Response.json({ deploy }, { status: 201 })
              : Response.json({ repo: deploy.repo, deploys: [deploy] });
          }
        ),
        { preconnect: fetch.preconnect }
      ),
    }
  );
  return { code, output, requests, ledger: join(root, "sessions", "deploy-cli", "results.jsonl") };
}

test("a valid deploy dry-run writes nothing and an invalid one journals its refusal", async () => {
  const result = await cli([...record, "--dry-run"]);
  expect(result.code, result.output).toBe(0);
  expect(JSON.parse(result.output)).toEqual({ repo: deploy.repo, program: deploy.program, commit });
  expect(result.requests).toEqual([]);
  expect(existsSync(result.ledger)).toBe(false);
  for (const argv of [
    record.slice(0, -1),
    ["deploy", "--repo", deploy.repo, "--commit", commit],
    [...record, "--program", "Bad_program"],
  ]) {
    const refused = await cli([...argv, "--dry-run"]);
    expect(refused.code, refused.output).toBe(2);
    expect(refused.requests).toEqual([]);
    const entries = readFileSync(refused.ledger, "utf8")
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
    expect(entries).toHaveLength(1);
    expect(entries[0]).toMatchObject({ tool: "dispatch_deploy", error: refused.output.trimEnd() });
  }
});

test("dispatch deploy posts only its five fields and JSON is the whole wire answer", async () => {
  const result = await cli([
    ...record,
    "--deployed-at",
    deploy.deployed_at,
    "--url",
    deploy.url,
    "--json",
  ]);
  expect(result.code, result.output).toBe(0);
  expect(JSON.parse(result.output)).toEqual({ deploy });
  expect(result.requests).toEqual([
    {
      method: "POST",
      path: "/api/v1/deploys",
      query: "",
      body: {
        repo: deploy.repo,
        program: deploy.program,
        commit,
        deployed_at: deploy.deployed_at,
        url: deploy.url,
      },
    },
  ]);
});

test("dispatch deploys reads each program's build and prints its source and link", async () => {
  const result = await cli(["deploys", "--repo", deploy.repo]);
  expect(result.code, result.output).toBe(0);
  expect(result.requests).toEqual([
    { method: "GET", path: "/api/v1/deploys", query: "?repo=acme%2Fwidgets", body: null },
  ]);
  for (const value of [deploy.program, commit, deploy.deployed_at, deploy.source, deploy.url])
    expect(result.output).toContain(value);
  const json = await cli(["deploys", "--repo", deploy.repo, "--json"]);
  expect(json.code, json.output).toBe(0);
  expect(JSON.parse(json.output)).toEqual({ repo: deploy.repo, deploys: [deploy] });
});
