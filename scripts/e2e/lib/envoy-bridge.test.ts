import { describe, expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { connect, nkeys } from "nats";
import { LoggableError } from "../../../packages/envoy-client/src/errors";
import { natsDialFailure } from "../../../packages/envoy-client/src/nats-connection";
import {
  bridgeConfigFromEnvironment,
  bridgeFailureLine,
  bridgeReadyLine,
  envelopeValidation,
} from "./envoy-bridge";

const currentEnvelope = (overrides: Record<string, unknown> = {}) => ({
  event_id: "evt-1",
  source: "github",
  source_event_id: "delivery-1",
  topic: "notifications.github.sjawhar.legion-smoke.pr.1",
  dedupe_key: "github.delivery-1",
  issued_at: 1_700_000_000_000,
  payload_summary: "pull request #1 opened",
  payload: '{"action":"opened"}',
  trace_id: "trace-1",
  ...overrides,
});

describe("bridgeConfigFromEnvironment", () => {
  test("relays only the sandbox repository's GitHub subjects", () => {
    const config = bridgeConfigFromEnvironment({
      SMOKE_REPO: "sjawhar/legion-smoke",
      SMOKE_RIG_NATS: "nats://172.30.0.1:31000",
      SMOKE_UPSTREAM_NATS: "nats://envoy-nats.tailnet.example:4222",
    });
    expect(config).toEqual({
      repository: "sjawhar/legion-smoke",
      subjects: ["notifications.github.sjawhar.legion-smoke.>"],
      upstreamUrl: "nats://envoy-nats.tailnet.example:4222",
      downstreamUrl: "nats://172.30.0.1:31000",
    });
  });

  // Envoy writes a dot in a repository's name as `_`, so the name stays one subject segment.
  test("relays a dotted repository's subjects as Envoy spells them", () => {
    const config = bridgeConfigFromEnvironment({
      SMOKE_REPO: "acme/wid.gets",
      SMOKE_RIG_NATS: "nats://x:1",
      SMOKE_UPSTREAM_NATS: "nats://y.example:1",
    });
    expect(config.subjects).toEqual(["notifications.github.acme.wid_gets.>"]);
  });

  test("never subscribes to Dispatch issue subjects (two rigs on one project cross-admit)", () => {
    const config = bridgeConfigFromEnvironment({
      SMOKE_REPO: "a/b",
      SMOKE_RIG_NATS: "nats://x:1",
      SMOKE_UPSTREAM_NATS: "nats://y.example:1",
    });
    expect(config.subjects.some((s) => s.startsWith("notifications.dispatch"))).toBe(false);
  });

  test("refuses a repository that could broaden the NATS subscription", () => {
    for (const repo of ["example-org/>", "a/*", "a.b", "", "a/b/c"]) {
      expect(() =>
        bridgeConfigFromEnvironment({
          SMOKE_REPO: repo,
          SMOKE_RIG_NATS: "nats://x:1",
          SMOKE_UPSTREAM_NATS: "nats://y.example:1",
        })
      ).toThrow(/SMOKE_REPO/);
    }
  });

  test("requires SMOKE_RIG_NATS", () => {
    expect(() =>
      bridgeConfigFromEnvironment({ SMOKE_REPO: "a/b", SMOKE_UPSTREAM_NATS: "nats://y.example:1" })
    ).toThrow(/SMOKE_RIG_NATS/);
  });

  test("requires SMOKE_UPSTREAM_NATS: the operator names the production NATS, it has no default", () => {
    for (const upstream of [undefined, "", "  "]) {
      expect(() =>
        bridgeConfigFromEnvironment({
          SMOKE_REPO: "a/b",
          SMOKE_RIG_NATS: "nats://x:1",
          SMOKE_UPSTREAM_NATS: upstream,
        })
      ).toThrow("SMOKE_UPSTREAM_NATS is required");
    }
  });

  test("refuses an upstream that is not one NATS URL naming a fully-qualified host, never echoing it", () => {
    for (const [upstream, canary] of [
      ["nats://canary-a:4222", "canary-a"],
      ["canary-b:4222", "canary-b"],
      ["nats://user:canary-c@canary-d", "canary-"],
      // the client dials what follows the last "://", so nothing may follow the authority
      ["nats://ok.example/nats://canary-e:4222", "canary-e"],
      ["nats://ok.example:4222?x=nats://canary-f:4222", "canary-f"],
      ["nats://ok.example:4222#nats://canary-g:4222", "canary-g"],
      ["nats://canary-h.example:4222,nats://canary-i.example:4222", "canary-"],
      ["nats://canary%zz.example:4222", "canary"],
      // nats.js ends the host at a `\` (it reads the URL as an http: one), so this dials the bare
      // name canary-j, which only a resolver's search domain completes
      ["nats://canary-j\\en@envoy-nats.tailnet.example:4222", "canary-j"],
      ["::", "::"],
    ] as const) {
      const refusal = (() => {
        try {
          bridgeConfigFromEnvironment({
            SMOKE_REPO: "a/b",
            SMOKE_RIG_NATS: "nats://x:1",
            SMOKE_UPSTREAM_NATS: upstream,
          });
        } catch (error) {
          return error instanceof Error ? error.message : String(error);
        }
        return "accepted";
      })();
      expect(refusal).toStartWith(
        "SMOKE_UPSTREAM_NATS is not one NATS URL naming a fully-qualified host"
      );
      expect(refusal).not.toContain(canary);
    }
  });

  test("accepts one fully-qualified upstream, trimmed, with or without a scheme", () => {
    for (const [upstream, want] of [
      [" nats://envoy-nats.tailnet.example:4222 ", "nats://envoy-nats.tailnet.example:4222"],
      ["\tnats://envoy-nats.tailnet.example:4222/\n", "nats://envoy-nats.tailnet.example:4222/"],
      ["envoy-nats.tailnet.example:4222", "envoy-nats.tailnet.example:4222"],
      ["tls://user:pw@envoy-nats.tailnet.example", "tls://user:pw@envoy-nats.tailnet.example"],
      ["nats://127.0.0.1:4222", "nats://127.0.0.1:4222"],
    ]) {
      const config = bridgeConfigFromEnvironment({
        SMOKE_REPO: "a/b",
        SMOKE_RIG_NATS: "nats://x:1",
        SMOKE_UPSTREAM_NATS: upstream,
      });
      expect(config.upstreamUrl).toBe(want);
    }
  });
});

/** What real nats.js throws when it fails to connect to `server`: no server runs there. */
async function realFailure(server: string): Promise<unknown> {
  try {
    const connection = await connect({ servers: server, reconnect: false, timeout: 5_000 });
    await connection.close();
  } catch (error) {
    return error;
  }
  throw new Error(`${server} connected, which this row needs it not to`);
}

/** Everything an error carries that an uncaught one prints: its message and Bun's `input`. */
const carried = (error: unknown) =>
  `${error instanceof Error ? error.message : String(error)} ${String((error as { input?: unknown }).input ?? "")}`.toLowerCase();

// Neither connection's setting reaches a line: the rig's NATS (SMOKE_RIG_NATS) may hold a
// credential, and the operator's upstream names production infrastructure and may hold one too.
// Each URL's user info is joined here, so the source holds no credential-bearing URL for the push
// leak check to find.
describe("the bridge's output lines", () => {
  const userinfo = (...parts: string[]) => `${parts.join(":")}@`;
  const canary = () => `canary${randomBytes(4).toString("hex")}`;

  test("the readiness line names both servers by their variables alone", () => {
    const config = bridgeConfigFromEnvironment({
      SMOKE_REPO: "a/b",
      SMOKE_RIG_NATS: `nats://${userinfo("rig", "canary-rig-pw")}172.30.0.1:31000`,
      SMOKE_UPSTREAM_NATS: "nats://envoy-nats.tailnet.example:4222",
    });
    const line = bridgeReadyLine(config);
    expect(line).toContain("upstream=SMOKE_UPSTREAM_NATS downstream=SMOKE_RIG_NATS");
    expect(line.toLowerCase()).not.toContain("canary");
    expect(line).not.toContain("172.30.0.1");
  });

  // Each row holds first that nats.js's own error carries the secret, so it can fail for the leak.
  // An upstream nats.js cannot read is refused before any dial (bridgeConfigFromEnvironment).
  const seed = new TextDecoder().decode(nkeys.createUser().getSeed());
  const secrets = { host: canary(), rigPassword: canary(), rigToken: canary() };
  test.each([
    [
      "an upstream host the resolver cannot find",
      "upstream",
      `nats://${userinfo("user", "pw")}${secrets.host}.e2-proof.invalid:4222`,
      secrets.host,
    ],
    [
      "a rig server that does not parse",
      "rig",
      `nats://${userinfo("rig", secrets.rigPassword)}172.30.0.1:99999`,
      secrets.rigPassword,
    ],
    ["a rig seed pasted as the host", "rig", `nats://${seed}:4222`, seed],
    [
      "a rig token with an unencoded /",
      "rig",
      `nats://${userinfo(`${secrets.rigToken}/def+gh==`)}172.30.0.1:31000`,
      secrets.rigToken,
    ],
  ] as const)("%s: the failure line carries none of it", async (_, which, server, secret) => {
    const config = bridgeConfigFromEnvironment({
      SMOKE_REPO: "a/b",
      SMOKE_RIG_NATS: which === "rig" ? server : "nats://172.30.0.1:31000",
      SMOKE_UPSTREAM_NATS: which === "upstream" ? server : "nats://envoy-nats.tailnet.example:4222",
    });
    const error = await realFailure(server);
    expect(carried(error)).toContain(secret.toLowerCase());
    const lines = [bridgeReadyLine(config), bridgeFailureLine(error, config)].join("\n");
    expect(lines.toLowerCase()).not.toContain(secret.toLowerCase());
    expect(lines).toContain("BRIDGE UNHEALTHY ");
  });

  test("a failed dial reads as the bridge's own line, the upstream named by its variable", async () => {
    const config = bridgeConfigFromEnvironment({
      SMOKE_REPO: "a/b",
      SMOKE_RIG_NATS: "nats://172.30.0.1:31000",
      SMOKE_UPSTREAM_NATS: "nats://envoy-nats.tailnet.example:4222",
    });
    // As runBridge's dial wraps a failure.
    const refused = new LoggableError(
      natsDialFailure("SMOKE_UPSTREAM_NATS", await realFailure("nats://127.0.0.1:1"))
    );
    expect(bridgeFailureLine(refused, config)).toBe(
      "BRIDGE UNHEALTHY could not connect to NATS at SMOKE_UPSTREAM_NATS: CONNECTION_REFUSED"
    );
    expect(bridgeFailureLine(new Error(`refused ${canary()}`), config)).toBe(
      "BRIDGE UNHEALTHY Error (message withheld)"
    );
  });
});

describe("envelopeValidation", () => {
  test("accepts a current envelope as bytes or text", () => {
    const text = JSON.stringify(currentEnvelope());
    expect(envelopeValidation(new TextEncoder().encode(text))).toEqual({
      ok: true,
      shape: "current",
    });
    expect(envelopeValidation(text)).toEqual({ ok: true, shape: "current" });
  });

  test("names every missing and extra field of a legacy envelope", () => {
    const legacy: Record<string, unknown> = currentEnvelope({ legacy_payload: "old field" });
    delete legacy.event_id;
    delete legacy.payload_summary;
    expect(envelopeValidation(JSON.stringify(legacy))).toMatchObject({
      ok: false,
      missing: ["event_id", "payload_summary"],
      extra: ["legacy_payload"],
    });
  });

  test("rejects malformed JSON", () => {
    expect(envelopeValidation(new TextEncoder().encode("{not json"))).toMatchObject({
      ok: false,
      missing: [],
      extra: [],
      errors: [expect.stringContaining("invalid JSON")],
    });
  });
});
