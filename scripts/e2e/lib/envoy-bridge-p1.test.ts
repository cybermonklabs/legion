// The acceptance's test (AcceptE2-2) of the bridge's failure line against the digits of
// SMOKE_UPSTREAM_NATS's password. nats.js reads the first `:` followed by digits as the port, so a
// password of six digits before an IP host is a port Bun refuses, and its text names the password
// ("Port should be >= 0 and < 65536. Received 547204"). At 0e663ccef8, whose bridge replaced the
// user info whole and printed the rest of nats.js's text, both rows failed: the line held the
// password. The line now prints the error's code alone (natsFailureReason).
import { describe, expect, test } from "bun:test";
import { randomInt } from "node:crypto";
import { connect } from "nats";
import { bridgeConfigFromEnvironment, bridgeFailureLine } from "./envoy-bridge";

const sixDigits = () => String(randomInt(100000, 1000000));

async function realError(server: string): Promise<unknown> {
  try {
    const connection = await connect({ servers: server, reconnect: false, timeout: 5_000 });
    await connection.close();
  } catch (error) {
    return error;
  }
  throw new Error(`${server} connected, which this row needs it not to`);
}

describe("the bridge's failure line names no digit of the upstream's password", () => {
  test.each([
    ["an IP host with a port", (password: string) => `nats://operator:${password}@127.0.0.1:4222`],
    ["an IP host with no port", (password: string) => `nats://operator:${password}@10.255.255.1`],
  ])("%s", async (_, spelling) => {
    const password = sixDigits();
    const upstream = spelling(password);
    const config = bridgeConfigFromEnvironment({
      SMOKE_REPO: "acme/widgets",
      SMOKE_RIG_NATS: "nats://127.0.0.1:1",
      SMOKE_UPSTREAM_NATS: upstream,
    });
    const error = await realError(upstream);
    // The row can fail for the leak: nats.js's own text carries the password.
    expect(error instanceof Error ? error.message : String(error)).toContain(password);
    expect(bridgeFailureLine(error, config)).not.toMatch(new RegExp(`(?<!\\d)${password}(?!\\d)`));
  });
});
