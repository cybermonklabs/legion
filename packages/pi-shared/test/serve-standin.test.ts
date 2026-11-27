import { afterEach, expect, spyOn, test } from "bun:test";
import { type Cleanup, serveStandin } from "./omp-harness";

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

// A request that reaches the stand-in although the row's clients did not send it, an OAuth
// client's form body as one reached a real-binary row's stand-in, beside one the row's own client
// sends. The stranger's is refused by name and left out of what the row reads; the row's own is
// recorded and answered as before.
test("a stranger's form body is refused by name, and neither recorded nor handed to the case's answer", async () => {
  const printed = spyOn(console, "error").mockImplementation(() => {});
  cleanup.push(async () => printed.mockRestore());
  const answered: string[] = [];
  const { requests, base } = serveStandin(cleanup, (url) => {
    answered.push(url.pathname);
    return Response.json({ answered: url.pathname });
  });

  const stranger = await fetch(`${base}/v1/oauth/token`, {
    method: "POST",
    body: new URLSearchParams({ client_id: "stranger", grant_type: "refresh_token" }),
  });
  const refusal: unknown = await stranger.json();
  const own = await fetch(`${base}/v1/interests/subscribe`, {
    method: "POST",
    body: JSON.stringify({ session_id: "ses_own" }),
  });

  expect(stranger.status).toBe(400);
  expect(printed).toHaveBeenCalledTimes(1);
  const [line] = printed.mock.calls[0] ?? [];
  expect(String(line)).toStartWith(
    `the stand-in at ${base} refused POST /v1/oauth/token: its body is not JSON`
  );
  expect(refusal).toEqual({ error: line });
  expect(await own.json()).toEqual({ answered: "/v1/interests/subscribe" });
  expect(requests).toEqual([{ path: "/v1/interests/subscribe", body: { session_id: "ses_own" } }]);
  expect(answered).toEqual(["/v1/interests/subscribe"]);
});

// How that stranger reached the row: it dialed 127.0.0.1 at a port its own server once held there,
// which the kernel had given the stand-in. The stand-in listens at an address of its own, so the
// same dial reaches nothing of the row's. Another process may hold 127.0.0.1 at that port and
// answer, so what the row reads is the stand-in's own record.
test.skipIf(process.platform !== "linux")(
  "a client that dials 127.0.0.1 at the stand-in's port reaches nothing the row reads",
  async () => {
    const answered: string[] = [];
    const { requests, base } = serveStandin(cleanup, (url) => {
      answered.push(url.pathname);
      return Response.json({});
    });
    const at = new URL(base);
    expect(at.hostname).not.toBe("127.0.0.1");
    await fetch(`http://127.0.0.1:${at.port}/v1/oauth/token`, {
      method: "POST",
      body: JSON.stringify({ client_id: "stranger" }),
    }).then(
      (answer) => answer.body?.cancel(),
      () => undefined
    );
    await fetch(`${base}/v1/interests/subscribe`, { method: "POST", body: "{}" });
    expect(requests).toEqual([{ path: "/v1/interests/subscribe", body: {} }]);
    expect(answered).toEqual(["/v1/interests/subscribe"]);
  }
);
