import { expect, test } from "bun:test";
import { askJev, type JevAsk } from "../models";

/**
 * The retry ladder of one call (`send` in `models.ts`) through `askJev`, its real caller, against
 * a stand-in endpoint: the global `fetch` replaced for the row, and a stand-in key in the
 * environment. The ladder waits its first rung, about two seconds, before the second try.
 */

const ASK: JevAsk = {
  model: "jev-latest",
  state: "the case",
  options: {
    instructions: "Choose one.",
    choices: [
      { name: "approve", label: "Approve", description: "the document is ready" },
      { name: "changes", label: "Request changes", description: "the document is not ready" },
    ],
  },
};

// A 408 is an outage (`@legion/contracts/http-status`): the endpoint did not receive the whole
// request in time, and the same request may be answered when it is sent again.
test("a call the endpoint answered 408 is tried again, and the second answer is read", async () => {
  const held = { fetch: globalThis.fetch, key: process.env.TYPESAFE_API_KEY };
  const answers = [
    new Response("<html>408 Request Timeout</html>", { status: 408 }),
    Response.json({
      model: "jev-2026-11",
      answers: { verdict: { choice: "approve", confidence: 0.9 } },
    }),
  ];
  let calls = 0;
  globalThis.fetch = (async () => {
    calls += 1;
    const answer = answers.shift();
    if (answer === undefined) throw new Error("the stand-in endpoint was asked more than twice");
    return answer;
  }) as unknown as typeof fetch;
  const standIn = "stand-in-secret-408-retry-row";
  process.env.TYPESAFE_API_KEY = standIn;
  try {
    const answer = await askJev(ASK);
    expect(calls).toBe(2);
    expect(answer).toMatchObject({ chosen: ["Approve"], attempts: 2, fault: null });
  } finally {
    globalThis.fetch = held.fetch;
    if (held.key === undefined) delete process.env.TYPESAFE_API_KEY;
    else process.env.TYPESAFE_API_KEY = held.key;
  }
}, 20_000);
