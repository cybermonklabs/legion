import { afterAll, describe, expect, test } from "bun:test";
import { randomBytes, randomInt } from "node:crypto";
import dns from "node:dns";
import { connect, ErrorCode, NatsError, nkeys } from "nats";
import { LoggableError, natsFailureReason } from "../errors";
import { natsDialedAddress, natsDialFailure, natsServersForLog } from "../nats-connection";

// What a log prints of a failed NATS dial: the servers by the setting that named them
// (natsServersForLog), never by their text, and why the dial failed (natsFailureReason), an
// allowlist: nats.js's code, nats-server's fixed refusal, or a message this package composed,
// never the text nats.js, the runtime or a peer derived from what the servers were set to or from
// what the client sent.
//
// Each URL's user info is joined here, so the source holds no credential-bearing URL for the push
// leak check to find.
const userinfo = (...parts: string[]) => `${parts.join(":")}@`;
const canary = () => `canary${randomBytes(4).toString("hex")}`;
const sixDigits = () => String(randomInt(100000, 1000000));

/** The calls the stand-in resolver answered during the last {@link realFailure}, each as
 *  `<syscall> <name>`. */
let resolverAsked: string[] = [];

/**
 * A stand-in for node:dns's `syscall` call that answers every name as the system's resolver
 * answers one it cannot find, and keeps what it was asked. nats.js asks `resolve4` and `resolve6`
 * (`queryA`, `queryAaaa`) before its dial, and Bun's node:net asks `lookup` (`getaddrinfo`) for the
 * dial itself. Measured on Bun 1.4.4, each real answer is a DNSException `<syscall> ENOTFOUND
 * <name>`, with that code and syscall, so a stand-in's error names the server as a real one does.
 */
function unresolved(syscall: string) {
  return (hostname: string, ...rest: unknown[]) => {
    resolverAsked.push(`${syscall} ${hostname}`);
    const callback = rest.at(-1);
    if (typeof callback !== "function") throw new Error(`${syscall} was called with no callback`);
    const error = Object.assign(new Error(`${syscall} ENOTFOUND ${hostname}`), {
      name: "DNSException",
      code: "ENOTFOUND",
      syscall,
      hostname,
      errno: 4,
    });
    process.nextTick(() => callback(error));
  };
}

/** What real nats.js throws when it fails to connect to `server`: no server runs, so each fails
 *  at the URL parser, the resolver or the socket, and the error is nats.js's own. The resolver is
 *  the stand-in {@link unresolved}, so no row asks a DNS server and none waits on how fast the
 *  machine's DNS answers; an address is dialed for real. */
async function realFailure(server: string, timeout = 5_000): Promise<unknown> {
  const real = { lookup: dns.lookup, resolve4: dns.resolve4, resolve6: dns.resolve6 };
  resolverAsked = [];
  Object.assign(dns, {
    lookup: unresolved("getaddrinfo"),
    resolve4: unresolved("queryA"),
    resolve6: unresolved("queryAaaa"),
  });
  try {
    const connection = await connect({ servers: [server], reconnect: false, timeout });
    await connection.close();
  } catch (error) {
    return error;
  } finally {
    Object.assign(dns, real);
  }
  throw new Error(`${server} connected, which this row needs it not to`);
}

/** Everything nats.js's own error carries that an uncaught one prints: its message and Bun's
 *  `input`, lowercased, as a resolver lowercases the host it names. */
const carried = (error: unknown) =>
  `${error instanceof Error ? error.message : String(error)} ${String((error as { input?: unknown }).input ?? "")}`.toLowerCase();

/** The line every caller prints for a failed dial at the servers ENVOY_NATS_URL names. */
const failureLine = (error: unknown) =>
  natsDialFailure(natsServersForLog("ENVOY_NATS_URL", 1), error, "no credential");

/** A reason a log may print for a dial that failed at the transport: one code, or a syscall and
 *  its code. Nothing in it is any part of what the server was set to. */
const CODE_REASON = /^(?:[a-z]+ )?[A-Za-z][A-Za-z0-9_]*$/;

// Every spelling a red team found where nats.js dials, and its error then names, something a
// rule on the server's spelling let through (five rounds of them: `/ ? #`, `\`, commas, percent
// escapes, Unicode, mixed case, digits read as the port, hyphens, special schemes, one-case
// secrets). Each row runs real nats.js and holds first that nats.js's own error carries the
// secret, so it can fail for the leak. The line it prints is then the fixed form, the setting's
// name and a code, and holds none of the secret.
describe("a failed dial's line holds no part of the server", () => {
  const seed = new TextDecoder().decode(nkeys.createUser().getSeed());
  const tokens = {
    backslash: canary(),
    comma: canary(),
    percent: canary(),
    unicode: canary(),
    slash: canary(),
    special: `Xk3pQ9vT2mL8wR5n${randomBytes(3).toString("hex")}`,
    specialHyphen: `Xk3pQ9vT-2mL8wR5n${randomBytes(3).toString("hex")}`,
    lower: `ahth4eiquaiv8ohg${randomBytes(2).toString("hex")}`,
    split: `k7wq2m9x${randomBytes(2).toString("hex")}`,
  };
  const digits = {
    ipv4: sixDigits(),
    loopback: sixDigits(),
    second: sixDigits(),
    ipv6: sixDigits(),
  };
  const mixed = `Xk3pQ9vT2mL8wR5n${randomBytes(3).toString("hex")}`;
  const hyphenated = `Xk3pQ9vT-2mL8wR5n${randomBytes(3).toString("hex")}`;
  const unicodeHost = (scheme: string) => `${scheme}Grüße${tokens.unicode}Secret:4222`;
  const unicodeSecrets = [
    `grüße${tokens.unicode}secret`,
    new URL(`http://Grüße${tokens.unicode}Secret`).hostname,
  ];
  const rows: [label: string, server: string, secrets: readonly string[]][] = [
    [
      "a backslash in the user info",
      `nats://${userinfo(`${tokens.backslash}\\en`)}127.0.0.1:4222`,
      [tokens.backslash],
    ],
    [
      "a comma list in one entry, a token in the second",
      `nats://a.example:4222,nats://${userinfo(`${tokens.comma}/x`)}127.0.0.1:4222`,
      [tokens.comma],
    ],
    [
      "a comma list in one entry, a seed in the second",
      `nats://a.example:4222,nats://${seed}:4222`,
      [seed],
    ],
    [
      "a percent escape in the user info",
      `nats://${userinfo(`${tokens.percent}%41/x`)}127.0.0.1:4222`,
      [tokens.percent],
    ],
    ["a Unicode name", unicodeHost("nats://"), unicodeSecrets],
    [
      "a token holding an unencoded /",
      `nats://${userinfo(`${tokens.slash}/def+gh==`)}127.0.0.1:4222`,
      [tokens.slash],
    ],
    ["an nkey seed pasted as the server", `nats://${seed}:4222`, [seed]],
    ["a mixed-case token pasted as the server", `nats://${mixed}:4222`, [mixed]],
    [
      "a hyphenated mixed-case token pasted as the server",
      `nats://${hyphenated}:4222`,
      [hyphenated],
    ],
    [
      "a password's six digits on an IP host",
      `nats://${userinfo("user", `${digits.ipv4}abc`)}10.0.0.5:4222`,
      [digits.ipv4],
    ],
    [
      "a password's six digits on the loopback",
      `nats://${userinfo("user", `${digits.loopback}abc`)}127.0.0.1:4222`,
      [digits.loopback],
    ],
    [
      "a password's six digits after its second `:`",
      `nats://${userinfo("user", "abc", `${digits.second}x`)}10.0.0.5:4222`,
      [digits.second],
    ],
    [
      "a password's six digits after a `]:`, before an IPv6 host",
      `nats://${userinfo("user", `pw]:${digits.ipv6}`)}[2001:db8::1]:4222`,
      [digits.ipv6],
    ],
    // B1: a special scheme lowercases and punycodes the host before any rule on its case reads it.
    ["a token under ws://", `ws://${tokens.special}:4222`, [tokens.special]],
    ["a token under wss://", `wss://${tokens.special}:4222`, [tokens.special]],
    ["a token under http://", `http://${tokens.special}:4222`, [tokens.special]],
    ["a token under https://", `https://${tokens.special}:4222`, [tokens.special]],
    ["a token under WS://", `WS://${tokens.special}:4222`, [tokens.special]],
    ["a token under ws:// with no port", `ws://${tokens.special}`, [tokens.special]],
    [
      "a hyphenated token under wss://",
      `wss://${tokens.specialHyphen}:4222`,
      [tokens.specialHyphen],
    ],
    ["a Unicode name under ws://", unicodeHost("ws://"), unicodeSecrets],
    // F6 and F5: a secret in one case, and the first half of a token split at its comma.
    ["a one-case token pasted as the server", `nats://${tokens.lower}:4222`, [tokens.lower]],
    [
      "the first half of a token ENVOY_NATS_URL split at its comma",
      `nats://${tokens.split}`,
      [tokens.split],
    ],
  ];
  test.each(rows)(
    "%s",
    async (_, server, secrets) => {
      const error = await realFailure(server);
      expect(secrets.some((secret) => carried(error).includes(secret.toLowerCase()))).toBe(true);
      const reason = natsFailureReason(error);
      expect(reason).toMatch(CODE_REASON);
      const line = failureLine(error);
      expect(line).toBe(
        `could not connect to NATS at the server ENVOY_NATS_URL names presenting no credential: ${reason}`
      );
      for (const secret of secrets) expect(line.toLowerCase()).not.toContain(secret.toLowerCase());
    },
    30_000
  );
});

// A server that takes the connection and never sends its INFO, for a dial that times out.
const mute = Bun.listen({ hostname: "127.0.0.1", port: 0, socket: { data() {} } });

/** A peer that speaks enough of NATS to send a nonce, and answers the CONNECT with the `-ERR`
 *  `answer` makes of the CONNECT's own text. */
function refusingPeer(answer: (connect: string) => string) {
  return Bun.listen({
    hostname: "127.0.0.1",
    port: 0,
    socket: {
      open(socket) {
        socket.write(
          'INFO {"server_id":"x","version":"2.10.0","proto":1,"nonce":"abc","max_payload":1048576}\r\n'
        );
      },
      data(socket, data) {
        const sent = String(data).match(/CONNECT (\{.*\})/);
        if (sent?.[1] !== undefined) socket.write(`-ERR ${answer(sent[1])}\r\n`);
      },
    },
  });
}
const peers: { stop(closeActive?: boolean): void }[] = [mute];
afterAll(() => {
  for (const peer of peers) peer.stop(true);
});

/** What real nats.js throws when the peer `answer` makes refuses a CONNECT presenting `token`. */
async function refusedBy(answer: (connect: string) => string, token: string): Promise<unknown> {
  const peer = refusingPeer(answer);
  peers.push(peer);
  try {
    const connection = await connect({
      servers: `nats://127.0.0.1:${peer.port}`,
      reconnect: false,
      timeout: 3_000,
      authenticator: () => ({ auth_token: token, sig: "x" }),
    });
    await connection.close();
  } catch (error) {
    return error;
  }
  throw new Error("the peer admitted the connection, which this row needs it not to");
}

// What an operator still reads: each error nats.js and the runtime give a failed dial, as the line
// prints it.
describe("a failed dial's line says why it failed", () => {
  test("a server that refuses the connection: CONNECTION_REFUSED", async () => {
    const error = await realFailure("nats://127.0.0.1:1");
    expect(failureLine(error)).toBe(
      "could not connect to NATS at the server ENVOY_NATS_URL names presenting no credential: CONNECTION_REFUSED"
    );
  }, 30_000);

  test("a server that never answers: TIMEOUT", async () => {
    const error = await realFailure(`nats://127.0.0.1:${mute.port}`, 500);
    expect(natsFailureReason(error)).toBe("TIMEOUT");
  }, 30_000);

  test("a server that does not parse: ERR_INVALID_URL, and its URL is not quoted", async () => {
    const error = await realFailure(`nats://${userinfo("user", "canary-pw")}canary-host:99999`);
    // Bun keeps the URL it could not parse in the error's `input`, which an uncaught error prints.
    expect(carried(error)).toContain("canary-pw");
    expect(natsFailureReason(error)).toBe("ERR_INVALID_URL");
    expect(failureLine(error)).not.toContain("canary");
  }, 30_000);

  test("a port the socket refuses: ERR_SOCKET_BAD_PORT, and not its number", async () => {
    const error = await realFailure(`nats://${userinfo("user", sixDigits())}127.0.0.1:4222`);
    expect(natsFailureReason(error)).toMatch(/^(?:[a-z]+ )?ERR_SOCKET_BAD_PORT$/);
  }, 30_000);

  test("a name the resolver cannot find: getaddrinfo and its code, and not the name", async () => {
    const name = `${canary()}.invalid`;
    const error = await realFailure(`nats://${name}:4222`);
    expect(resolverAsked).toContain(`getaddrinfo ${name}`);
    expect(carried(error)).toContain(name);
    expect(natsFailureReason(error)).toBe("getaddrinfo ENOTFOUND");
  }, 30_000);

  test("nats-server's own refusal: its fixed words", async () => {
    for (const fixed of [
      "Authorization Violation",
      "User Authentication Expired",
      "Account Authentication Expired",
    ]) {
      const error = await refusedBy(() => `'${fixed}'`, canary());
      expect(natsFailureReason(error)).toBe(fixed);
    }
    // nats.js ends a dial that saw this one with its own TIMEOUT, so it is built here as
    // protocol.js `toError` builds it on an established connection.
    const timedOut = new NatsError("'Authentication Timeout'", ErrorCode.AuthenticationTimeout);
    expect(natsFailureReason(timedOut)).toBe("Authentication Timeout");
  }, 30_000);

  // S1: a peer that is no nats-server can fill the refusal with the CONNECT it was sent, the
  // broker token included; nats.js keeps any `-ERR` text that holds the refusal's words.
  test("a refusal that echoes the CONNECT: its text withheld, and no token in the line", async () => {
    const token = `CANARYTOKEN${randomBytes(6).toString("hex")}`;
    const error = await refusedBy((sent) => `'Authorization Violation ${sent}'`, token);
    expect(carried(error)).toContain(token.toLowerCase());
    expect(natsFailureReason(error)).toBe(
      "the server refused the connection (its text is withheld)"
    );
    const line = natsDialFailure(
      natsServersForLog("ENVOY_NATS_URL", 1),
      error,
      "the session's broker token"
    );
    expect(line).not.toContain(token);
  }, 30_000);

  test("any other text a server sent: withheld, under each code nats.js gives it", () => {
    const echoed = canary();
    for (const code of [
      ErrorCode.AuthorizationViolation,
      ErrorCode.AuthenticationExpired,
      ErrorCode.AccountExpired,
      ErrorCode.AuthenticationTimeout,
      ErrorCode.ProtocolError,
    ]) {
      expect(natsFailureReason(new NatsError(`'${echoed}'`, code))).toBe(
        "the server refused the connection (its text is withheld)"
      );
    }
    const denied = new NatsError(
      `Permissions Violation for Subscription to "${echoed}"`,
      ErrorCode.PermissionsViolation
    );
    expect(natsFailureReason(denied)).toBe(
      "the server refused a permission (its text is withheld)"
    );
  });

  test("any other nats.js error: its code alone, whatever its text", () => {
    // nats.js's TLS errors carry the runtime's text (node_transport.js), a certificate's names in it.
    const tls = new NatsError(`certificate names ${canary()}.example`, ErrorCode.Tls);
    expect(natsFailureReason(tls)).toBe("TLS");
  });

  test("this package's own reason: its message whole, and no cause kept", () => {
    const own = new LoggableError("NATS_AUTH is broker, but the NATS server sent no nonce to sign");
    expect(natsFailureReason(own)).toBe(
      "NATS_AUTH is broker, but the NATS server sent no nonce to sign"
    );
    expect(own.cause).toBeUndefined();
  });

  test("an error of no kind it knows: its type, and its message withheld", () => {
    expect(natsFailureReason(new Error(`refused ${canary()}`))).toBe("Error (message withheld)");
    expect(natsFailureReason(new TypeError(`bad ${canary()}`))).toBe(
      "TypeError (message withheld)"
    );
    expect(natsFailureReason(`a thrown ${canary()}`)).toBe("a thrown string (message withheld)");
  });
});

describe("natsServersForLog", () => {
  test("names the servers by their setting and how many it named", () => {
    expect(natsServersForLog("ENVOY_NATS_URL", 1)).toBe("the server ENVOY_NATS_URL names");
    expect(natsServersForLog("envoy.natsUrls in /home/u/envoy.json", 2)).toBe(
      "the 2 servers envoy.natsUrls in /home/u/envoy.json names"
    );
  });
});

// The host nats.js dials, which the e2e bridge checks its upstream against.
describe("natsDialedAddress", () => {
  test("is the host and port nats.js itself dials", () => {
    expect(natsDialedAddress(`nats://${userinfo("u", "12345")}nats.example:4222`)).toEqual({
      host: "nats.example",
      port: 12345,
    });
    expect(natsDialedAddress("nats://Host.Example:4222")).toEqual({
      host: "host.example",
      port: 4222,
    });
    expect(natsDialedAddress("nats://host:99999")).toBeUndefined();
  });
});
