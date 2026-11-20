import { describe, expect, test } from "bun:test";
import { bearerCredential } from "../bearer";
import { DispatchClient } from "../dispatch-http";

// From the acceptance of #4140 at be17859eca (AcceptE2-2). be17859eca's A3 rules that every
// Dispatch write answers JSON or a bodiless 204 or 205, so a 2xx that is empty or not JSON is no
// answer of Dispatch's. A read of a JSON route is the same: Dispatch writes every such answer with
// WriteJSON. Today `#response` hands a read's non-JSON 2xx back as its text, cast to the route's
// type, so on the real stack, against a Dispatch URL that a captive portal or proxy answered with
// a 200 text page, `dispatch open-asks` and Claude Code's session hook (open-asks) printed
// "undefined unanswered asks you authored on active issues and project documents. Waiting on
// human (0):", which tells the session that nothing waits on it.
describe("a JSON read whose 2xx is not JSON is no answer of Dispatch's", () => {
  const answers: readonly (readonly [string, string, string])[] = [
    ["a 200 HTML page", "<html><body>Sign in to this network</body></html>", "text/html"],
    ["a 200 text page", "session closed", "text/plain"],
    ["an empty 200 that says JSON", "", "application/json"],
  ];
  for (const [label, body, type] of answers) {
    test(`${label} fails the read rather than being read as its answer`, async () => {
      const client = new DispatchClient(
        "https://dispatch.example",
        bearerCredential("secret"),
        (async () =>
          new Response(body, {
            status: 200,
            headers: { "content-type": type },
          })) as unknown as typeof fetch,
        { retryNoRoute: false }
      );
      await expect(client.openAsks("ses_acc")).rejects.toThrow();
    });
  }
  test("a JSON 200 is still read as the answer", async () => {
    const served = { human: [], agent: [] };
    const client = new DispatchClient(
      "https://dispatch.example",
      bearerCredential("secret"),
      (async () =>
        new Response(JSON.stringify(served), {
          status: 200,
          headers: { "content-type": "application/json" },
        })) as unknown as typeof fetch,
      { retryNoRoute: false }
    );
    expect(await client.openAsks("ses_acc")).toEqual(served as never);
  });
});
