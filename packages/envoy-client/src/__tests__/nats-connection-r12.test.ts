import { describe, expect, test } from "bun:test";
import { nkeys } from "nats";
import { natsConnectionOptions } from "../nats-connection";

// R12 of the auth service's spec (unchanged without the callout): a hub configured with no auth
// service, and clients configured with none, run exactly as they run today. Today (the merge base,
// 1610472cb1) a session that connects with a stored nkey seed, or with no credential, and that the
// hub refuses twice in a row closes for good: nats.js's default, `ignoreAuthErrorAbort: false`
// (options.js:50, acted on at protocol.js:657-661). No client of slice E1 presents a credential the
// auth service decides - the TypeScript broker credential is slice E2's - so this option changes
// only these clients, and only on a hub with no auth service.
//
// Measured at 40c57f5ee5 on nats:2.15.1 (testnats.HubImage) with static nkey users only, the hub
// restarted without the client's user for 12 s: natsConnectionOptions at the merge base closed
// with 'Authorization Violation' after 2 refusals; at the head it took 6 refusals and never closed.
const user = nkeys.createUser();
const seed = { value: new TextDecoder().decode(user.getSeed()), source: "test" };

describe("natsConnectionOptions without a credential the auth service decides (R12)", () => {
  test("a seeded connection gives up on a repeated refusal, as at the merge base", () => {
    const options = natsConnectionOptions({
      servers: ["nats://127.0.0.1:4222"],
      name: "test",
      credential: { kind: "seed", seed },
    });
    expect(options.ignoreAuthErrorAbort).not.toBe(true);
  });

  test("a connection with no credential gives up on a repeated refusal, as at the merge base", () => {
    const options = natsConnectionOptions({
      servers: ["nats://127.0.0.1:4222"],
      name: "test",
      credential: { kind: "none" },
    });
    expect(options.ignoreAuthErrorAbort).not.toBe(true);
  });
});
