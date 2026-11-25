import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  dispatchRepliesOf,
  readJson,
  resetRetryWaitUnitForTests,
  setRetryWaitUnitForTests,
} from "../dispatch-read";

/**
 * Where the approval-eval reads ask Dispatch: each API path joined onto `DISPATCH_URL`'s path,
 * its query kept, against a stand-in Dispatch served under a path and a query that answers 404
 * anywhere else, as a Dispatch behind a path-routed gateway would.
 */

const TOKEN = "fake-bearer";
const COMMENT = "3f1c2a4b-5d6e-4f70-8a9b-0c1d2e3f4a5b";
const ROUTE = `/api/v1/comments/${COMMENT}`;

/** Every request the stand-in took, as `<path><query>`. */
const asked: string[] = [];
/** Where the stand-in is served: under `mount`, with `query` on every request. */
let served = { mount: "", query: "" };

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  fetch(request) {
    const url = new URL(request.url);
    asked.push(`${url.pathname}${url.search}`);
    if (
      request.headers.get("authorization") === `Bearer ${TOKEN}` &&
      url.pathname === `${served.mount}${ROUTE}` &&
      url.search === served.query
    ) {
      return Response.json({ comment: { id: COMMENT, body: "the claim" }, replies: [] });
    }
    return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 });
  },
});
const origin = `http://127.0.0.1:${server.port}`;

const directory = temporaryDirectory("approval-eval-dispatch-read-url-join");
const before = { url: process.env.DISPATCH_URL, tokenFile: process.env.DISPATCH_TOKEN_FILE };

beforeAll(() => {
  const tokenFile = join(directory, "token");
  writeFileSync(tokenFile, `${TOKEN}\n`);
  process.env.DISPATCH_TOKEN_FILE = tokenFile;
  // A read that is not answered is tried again after waits a second long each; a millisecond
  // here, so a row that fails does not wait out the ladder.
  setRetryWaitUnitForTests(1);
});

afterAll(async () => {
  resetRetryWaitUnitForTests();
  await server.stop(true);
  if (before.url === undefined) delete process.env.DISPATCH_URL;
  else process.env.DISPATCH_URL = before.url;
  if (before.tokenFile === undefined) delete process.env.DISPATCH_TOKEN_FILE;
  else process.env.DISPATCH_TOKEN_FILE = before.tokenFile;
});

/** What `read` asked, the stand-in served under `mount` and `query`, and what it answered. */
async function askedBy(mount: string, query: string, read: () => Promise<unknown>) {
  served = { mount, query };
  asked.length = 0;
  const answer = await read().catch((error: unknown) => error);
  return { asked: [...asked], answer };
}

describe("dispatch-read's requests", () => {
  test("join the API path onto a configured URL's path and keep its query", async () => {
    const config = { baseUrl: `${origin}/prefix?key=k3y-made-up`, token: TOKEN };
    const { asked, answer } = await askedBy("/prefix", "?key=k3y-made-up", () =>
      readJson(ROUTE, config)
    );
    expect(asked).toEqual([`/prefix${ROUTE}?key=k3y-made-up`]);
    expect(answer).toEqual({ comment: { id: COMMENT, body: "the claim" }, replies: [] });
  });

  test("keep a DISPATCH_URL query whole, a trailing slash included", async () => {
    process.env.DISPATCH_URL = `${origin}/prefix/?key=k3y-made-up/`;
    const { asked, answer } = await askedBy("/prefix", "?key=k3y-made-up/", () =>
      dispatchRepliesOf(COMMENT)
    );
    expect(asked).toEqual([`/prefix${ROUTE}?key=k3y-made-up/`]);
    expect(answer).toEqual([]);
  });

  test("ask a plain origin at the API path itself", async () => {
    process.env.DISPATCH_URL = origin;
    const { asked, answer } = await askedBy("", "", () => dispatchRepliesOf(COMMENT));
    expect(asked).toEqual([ROUTE]);
    expect(answer).toEqual([]);
  });

  test("refuse a DISPATCH_URL that is no absolute URL, quoting none of it", async () => {
    process.env.DISPATCH_URL = "not-a-url?key=k3y-made-up";
    const { asked, answer } = await askedBy("", "", () => dispatchRepliesOf(COMMENT));
    expect(asked).toEqual([]);
    expect(answer).toBeInstanceOf(Error);
    expect((answer as Error).message).toBe(
      "DISPATCH_URL is not an absolute URL with a host (not printed)"
    );
  });
});
