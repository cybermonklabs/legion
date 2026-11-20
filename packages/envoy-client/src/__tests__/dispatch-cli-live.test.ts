import { afterEach, expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { dispatchCliLive, useDispatchCliDirectories } from "../../test/dispatch-cli-live";

// The writes the live files make over the server's own routes (`asMachine`, `asPerson` of
// test/dispatch-cli-live.ts), with a gateway in the server's place that answers each with its own
// page echoing the request back, as a misconfigured proxy does: the identity the request carried
// bare on a line of its own near the top of the page, where no `Authorization:` or `Bearer` stands
// before it and the excerpt's 120 characters still reach it, then the request line and every
// header. The refusal must name that body through the excerpt and never print the identity; the
// server's own error JSON prints as it came.

const directories = useDispatchCliDirectories();
const stops: (() => void)[] = [];

afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A server in Dispatch's place on a loopback port of its own: `answer` for every request, and
 *  the identity headers each request carried. */
function standIn(answer: (request: Request, carried: string) => Response) {
  const carried: { authorization: string; user: string }[] = [];
  const server = Bun.serve({
    fetch(request) {
      const authorization = request.headers.get("authorization") ?? "";
      const user = request.headers.get("x-dispatch-user") ?? "";
      carried.push({ authorization, user });
      return answer(request, authorization.replace(/^Bearer /, "") || user);
    },
    hostname: "127.0.0.1",
    port: 0,
  });
  stops.push(() => server.stop(true));
  return { carried, url: `http://127.0.0.1:${server.port}` };
}

/** The gateway's 502 page: `identity` bare, then the request echoed back. */
function echoed(request: Request, identity: string): Response {
  const url = new URL(request.url);
  const page = [
    "<html><head><title>502 Bad Gateway</title></head><body>",
    identity,
    `<pre>${request.method} ${url.pathname} HTTP/1.1`,
    ...[...request.headers].map(([name, value]) => `${name}: ${value}`),
    "</pre></body></html>",
  ].join("\n");
  return new Response(page, { headers: { "Content-Type": "text/html" }, status: 502 });
}

/** The harness over `url` with `token` as the stack's machine token, acting for `person`. */
function harness(url: string, token: string, person: string) {
  return dispatchCliLive({
    directories,
    person,
    session: "proxy-echo-session",
    spec: "The spec no row reads.",
    stack: () => ({ stop: async () => {}, token, url }),
  });
}

/** What `call` rejected with, as its message; a call that resolves fails the row. */
async function refusalOf(call: Promise<unknown>): Promise<string> {
  try {
    await call;
  } catch (error) {
    return error instanceof Error ? error.message : String(error);
  }
  throw new Error("the call resolved, and the row wants its refusal");
}

test("a machine write a gateway answers names its body through the excerpt, never the bearer", async () => {
  const token = fixtureSecret("machine");
  const dispatch = standIn(echoed);
  const refusal = await refusalOf(
    harness(dispatch.url, token, "alice").asMachine("POST", "/api/v1/gates", { state: "open" })
  );
  expect(dispatch.carried).toEqual([{ authorization: `Bearer ${token}`, user: "" }]);
  expect(refusal).not.toContain(token);
  expect(refusal).toStartWith(
    `POST /api/v1/gates: 502 a body that is not Dispatch's error JSON ("`
  );
});

test("a person's write a gateway answers names its body through the excerpt, never the identity it carried", async () => {
  const person = fixtureSecret("person");
  const dispatch = standIn(echoed);
  const refusal = await refusalOf(
    harness(dispatch.url, fixtureSecret("machine"), person).asPerson("POST", "/api/v1/issues", {
      title: "Echoed",
    })
  );
  expect(dispatch.carried).toEqual([{ authorization: "", user: person }]);
  expect(refusal).not.toContain(person);
  expect(refusal).toStartWith(
    `POST /api/v1/issues: 502 a body that is not Dispatch's error JSON ("`
  );
});

test("a write Dispatch itself refuses names Dispatch's error JSON as it came", async () => {
  const refused = JSON.stringify({ code: "ISSUE_NOT_FOUND", error: "no issue LEGION-9" });
  const dispatch = standIn(
    () => new Response(refused, { headers: { "Content-Type": "application/json" }, status: 404 })
  );
  const refusal = await refusalOf(
    harness(dispatch.url, fixtureSecret("machine"), "alice").asMachine(
      "PATCH",
      "/api/v1/issues/LEGION-9"
    )
  );
  expect(refusal).toBe(`PATCH /api/v1/issues/LEGION-9: 404 ${refused}`);
});
