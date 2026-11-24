import { afterAll, afterEach, beforeAll, expect, type Mock, spyOn, test } from "bun:test";
import type { Server } from "bun";
import { loopbackHost } from "./loopback-host";
import { readStandinBody } from "./standin-body";

// A stand-in as the real-binary harness stands one up: each request's body read here, a refusal
// answered as it is, and every other request recorded and answered with the body it carried.
const handed: { readonly path: string; readonly body: Record<string, unknown> }[] = [];
let server: Server<undefined>;
let printed: Mock<typeof console.error>;

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    hostname: loopbackHost(),
    async fetch(request) {
      const { body, refusal } = await readStandinBody(request);
      if (refusal !== undefined) return refusal;
      handed.push({ path: new URL(request.url).pathname, body });
      return Response.json({ body });
    },
  });
  printed = spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  handed.length = 0;
  printed.mockClear();
});

afterAll(async () => {
  printed.mockRestore();
  await server.stop(true);
});

function post(path: string, body: string | URLSearchParams, type?: string): Promise<Response> {
  return fetch(new URL(path, server.url), {
    method: "POST",
    body,
    headers: type === undefined ? undefined : { "content-type": type },
  });
}

// The request that reached a real-binary row's stand-in from another process on the machine: an
// OAuth client's form body. Parsed as JSON, it threw out of the server's fetch.
test("a form body is refused by its method and path, is printed, and reaches no answer", async () => {
  const form = new URLSearchParams({ client_id: "stranger", grant_type: "refresh_token" });
  const answer = await post("/v1/oauth/token", form);
  const error =
    `the stand-in at ${server.url.origin} refused POST /v1/oauth/token: its body is not JSON ` +
    `(application/x-www-form-urlencoded;charset=UTF-8, ${form.toString().length} characters)`;
  expect({ status: answer.status, body: await answer.json() }).toEqual({
    status: 400,
    body: { error },
  });
  expect(printed.mock.calls).toEqual([[error]]);
  expect(handed).toEqual([]);
});

test.each([
  ["an array", "[1, 2]"],
  ["null", "null"],
  ["a number", "7"],
  ["a string", '"text"'],
  ["true", "true"],
  ["false", "false"],
])("a JSON body that is %s is refused as no object", async (_, text) => {
  const answer = await post("/v1/interests/subscribe", text, "application/json");
  const error = `the stand-in at ${server.url.origin} refused POST /v1/interests/subscribe: its body is JSON but not an object`;
  expect({ status: answer.status, body: await answer.json() }).toEqual({
    status: 400,
    body: { error },
  });
  expect(printed.mock.calls).toEqual([[error]]);
  expect(handed).toEqual([]);
});

test("a JSON object is the body, and an empty POST and a GET are each the empty object", async () => {
  const sent = { session_id: "ses_1", topics: ["a"] };
  expect(
    await (await post("/v1/interests/subscribe", JSON.stringify(sent), "application/json")).json()
  ).toEqual({ body: sent });
  expect(await (await post("/legion/v1/claims/ready", "")).json()).toEqual({ body: {} });
  expect(await (await fetch(new URL("/v1/roles/r", server.url))).json()).toEqual({ body: {} });
  expect(handed).toEqual([
    { path: "/v1/interests/subscribe", body: sent },
    { path: "/legion/v1/claims/ready", body: {} },
    { path: "/v1/roles/r", body: {} },
  ]);
  expect(printed).not.toHaveBeenCalled();
});

test("a body read failure returns and prints a refusal instead of rejecting", async () => {
  const request = new Request(new URL("/v1/interests/subscribe", server.url).href, {
    method: "POST",
    body: new ReadableStream({
      start(controller) {
        controller.error(new Error("request interrupted"));
      },
    }),
  });
  const { refusal } = await readStandinBody(request);
  expect(refusal?.status).toBe(400);
  const error =
    `the stand-in at ${server.url.origin} refused POST /v1/interests/subscribe: ` +
    "its body could not be read (Error: request interrupted)";
  expect(await refusal?.json()).toEqual({ error });
  expect(printed.mock.calls).toEqual([[error]]);
});
