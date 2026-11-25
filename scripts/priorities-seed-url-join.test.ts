import { afterAll, describe, expect, test } from "bun:test";
import { bearerCredential } from "../packages/envoy-client/src/bearer";
import { dispatchDocumentRef } from "../packages/envoy-client/src/dispatch-owner";
import { seedPriorities } from "./priorities-seed";

/** A table of one row that names no issue, so a run reads the table and the priorities and writes
 *  one priority: each of the seed's routes but the issue ones, which take the same join. */
const TABLE = [
  "| \\# | Priority | Owner | Where |",
  "| --- | --- | --- | --- |",
  "| 1 | One priority | A controller | nothing to point at yet |",
  "",
].join("\n");

/** Every request the stand-in took, as `<method> <path><query>`. */
const asked: string[] = [];

/** Where the stand-in Dispatch is served: under `mount`, with `query` on every request. A request
 *  asked anywhere else is answered 404, as a Dispatch behind a path-routed gateway would be. */
let served = { mount: "", query: "" };

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    asked.push(`${request.method} ${url.pathname}${url.search}`);
    await request.text();
    const route = url.pathname.startsWith(`${served.mount}/`)
      ? url.pathname.slice(served.mount.length)
      : undefined;
    if (route === undefined || url.search !== served.query) {
      return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
    }
    if (request.method === "GET" && route === "/api/v1/projects/ACME/artifacts/table-md/text") {
      return Response.json({ markdown: TABLE, version: 1, token: "sha256:document" });
    }
    if (route === "/api/v1/priorities" && request.method === "GET") {
      return Response.json({ priorities: [] });
    }
    if (route === "/api/v1/priorities" && request.method === "POST") {
      return Response.json({ id: "one-priority" }, { status: 201 });
    }
    return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
  },
});

afterAll(() => server.stop(true));

/** A run of the seed against `url`, the stand-in served as `mount` and `query` say, and the
 *  requests it made, sorted: the table and the priorities are read at once. */
async function seedAt(url: string, mount: string, query: string) {
  served = { mount, query };
  asked.length = 0;
  const report = await seedPriorities(
    { url, credential: bearerCredential("token") },
    dispatchDocumentRef("ACME", "table-md")
  ).catch((error: unknown) => error);
  return { asked: [...asked].sort(), report };
}

describe("priorities-seed's requests", () => {
  test("join each route onto a configured URL's path and keep its query", async () => {
    const { asked, report } = await seedAt(
      `http://127.0.0.1:${server.port}/prefix?key=k3y-made-up`,
      "/prefix",
      "?key=k3y-made-up"
    );
    expect(asked).toEqual([
      "GET /prefix/api/v1/priorities?key=k3y-made-up",
      "GET /prefix/api/v1/projects/ACME/artifacts/table-md/text?key=k3y-made-up",
      "POST /prefix/api/v1/priorities?key=k3y-made-up",
    ]);
    expect(report).toMatchObject({ rows: [{ row: "1", priority: "one-priority", written: true }] });
  });

  test("ask a plain origin at each route itself", async () => {
    const { asked, report } = await seedAt(`http://127.0.0.1:${server.port}`, "", "");
    expect(asked).toEqual([
      "GET /api/v1/priorities",
      "GET /api/v1/projects/ACME/artifacts/table-md/text",
      "POST /api/v1/priorities",
    ]);
    expect(report).toMatchObject({ rows: [{ row: "1", priority: "one-priority", written: true }] });
  });
});
