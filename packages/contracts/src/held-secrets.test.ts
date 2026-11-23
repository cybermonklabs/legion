import { afterEach, describe, expect, jest, test } from "bun:test";
import { randomBytes } from "node:crypto";
import {
  forgetHeldSecrets,
  HELD_PAST_LAPSE_MS,
  holdSecret,
  holdUrlUserinfo,
  scrub,
  scrubError,
  WITHHELD,
} from "./held-secrets";

afterEach(forgetHeldSecrets);

const secret = () => `dsp_${randomBytes(24).toString("base64url")}`;

/** How many values and windows the process's one registry holds now. */
function registrySize(): { readonly values: number; readonly windows: number } {
  const registry = (globalThis as Record<symbol, unknown>)[
    Symbol.for("@legion/contracts/held-secrets")
  ] as { values: Set<string>; windows: Set<string> };
  return { values: registry.values.size, windows: registry.windows.size };
}

describe("how long a secret is held", () => {
  test("a process that trades all its life holds the tokens of the last hour, not every one", () => {
    // A session renews a broker token every few minutes, and each trade holds its subject token,
    // its actor token and the token it got: held for life, a week of trades is tens of MiB.
    const life = holdSecret(secret());
    const now = Date.now();
    for (let trade = 0; trade < 5_000; trade += 1) {
      holdSecret(secret(), now - HELD_PAST_LAPSE_MS - 1);
    }
    const lately = holdSecret(secret(), now);
    // The lapsed trades are forgotten, and their windows with them; what is held for life, and
    // what lapsed less than an hour ago, are held still.
    expect(registrySize().values).toBe(2);
    expect(registrySize().windows).toBe(2 * (life.length - 9));
    expect(scrub(`${life} ${lately}`)).toBe(`${WITHHELD} ${WITHHELD}`);
  });

  test("a forgotten secret is printed as it is, and a window it shared with a held one is not", () => {
    const lapsed = secret();
    // Held for life, and shares its first ten characters with the one that lapses.
    const kept = holdSecret(`${lapsed.slice(0, 10)}${secret()}`);
    holdSecret(lapsed, Date.now() - HELD_PAST_LAPSE_MS - 1);
    // Each hold forgets what has lapsed by then.
    holdSecret(secret());
    expect(scrub(`echo ${lapsed.slice(12)}`)).toBe(`echo ${lapsed.slice(12)}`);
    expect(scrub(`echo ${kept.slice(0, 10)}`)).toBe(`echo ${WITHHELD}`);
  });

  test("held again, a secret is held for the longer of the two, and for life where either says so", () => {
    // On the fake clock, which the registry's lapses are read by (`Date.now`): each is due to be
    // forgotten a second from now, and held again before then.
    jest.useFakeTimers();
    try {
      const soon = Date.now() - HELD_PAST_LAPSE_MS + 1_000;
      const renewed = holdSecret(secret(), soon);
      holdSecret(renewed, Date.now());
      const forLife = holdSecret(secret(), soon);
      holdSecret(forLife);
      // Held until an hour from now, then held again with a lapse long past: that shortens
      // nothing.
      const late = holdSecret(secret(), Date.now());
      holdSecret(late, Date.now() - HELD_PAST_LAPSE_MS - 1);
      const once = holdSecret(secret(), soon);
      // The control, held once with the same lapse: held a millisecond before its second is up,
      // and forgotten at it. Each hold forgets what has lapsed by then.
      jest.advanceTimersByTime(999);
      holdSecret(secret());
      expect(scrub(once)).toBe(WITHHELD);
      jest.advanceTimersByTime(1);
      holdSecret(secret());
      expect(scrub(once)).toBe(once);
      expect(scrub(renewed)).toBe(WITHHELD);
      expect(scrub(forLife)).toBe(WITHHELD);
      expect(scrub(late)).toBe(WITHHELD);
    } finally {
      jest.useRealTimers();
    }
  });
});

describe("scrub", () => {
  test("a line with no held secret comes back as itself, with or without secrets held", () => {
    const line = "Dispatch at https://dispatch.example is unreachable: ECONNREFUSED";
    expect(scrub(line)).toBe(line);
    holdSecret(secret());
    expect(scrub(line)).toBe(line);
  });

  test("a held secret is taken out whole, wherever it stands", () => {
    const held = holdSecret(secret());
    expect(scrub(`refused ${held} twice: ${held}.`)).toBe(
      `refused ${WITHHELD} twice: ${WITHHELD}.`
    );
  });

  test("a run of 10 or more of its characters is taken out, and a shorter one is not", () => {
    const held = holdSecret(secret());
    const run = held.slice(5, 17);
    expect(scrub(`echo [${run}]`)).toBe(`echo [${WITHHELD}]`);
    expect(scrub(`echo [${held.slice(3, 13)}]`)).toBe(`echo [${WITHHELD}]`);
    expect(scrub(`echo [${held.slice(3, 12)}]`)).toBe(`echo [${held.slice(3, 12)}]`);
  });

  test("two held secrets side by side are one stretch withheld", () => {
    const one = holdSecret(secret());
    const two = holdSecret(secret());
    expect(scrub(`${one}${two}`)).toBe(WITHHELD);
  });

  test("a value shorter than 10 characters is not held", () => {
    holdSecret("token");
    expect(scrub("a token in the line")).toBe("a token in the line");
  });

  test("an error's message, stack and cause are scrubbed", () => {
    const held = holdSecret(secret());
    const error = new Error(`outer ${held}`, { cause: new Error(`inner ${held.slice(2, 20)}`) });
    scrubError(error);
    expect(error.message).toBe(`outer ${WITHHELD}`);
    expect(String(error.stack)).not.toContain(held);
    expect((error.cause as Error).message).toBe(`inner ${WITHHELD}`);
  });

  // Bun's console prints an error's own fields beside its message, so they are scrubbed too: an
  // `EnvoyApiError`'s `details.responseBody` is the listener's raw body (#4140, R-b).
  test("an error's own fields, and the plain objects it holds, are scrubbed", () => {
    const held = holdSecret(secret());
    const error = Object.assign(new Error("the listener refused"), {
      details: { status: 401, responseBody: `you sent ${held}`, url: "http://listener" },
      body: `echo ${held.slice(3, 17)}`,
      list: [`one ${held}`],
    });
    scrubError(error);
    expect(error.details.responseBody).toBe(`you sent ${WITHHELD}`);
    expect(error.details.url).toBe("http://listener");
    expect(error.body).toBe(`echo ${WITHHELD}`);
    expect(error.list).toEqual([`one ${WITHHELD}`]);
    expect(Bun.inspect(error)).not.toContain(held.slice(3, 17));
  });
});

// A URL's user info is sent by Bun's fetch as `Authorization: Basic <base64 of user:password>`,
// decoded, so a server that echoes the header or the URL prints neither once the URL is held.
describe("holdUrlUserinfo", () => {
  test("holds the password, the decoded pair and the Basic value", () => {
    const password = `pw-${randomBytes(9).toString("hex")}`;
    holdUrlUserinfo(
      `https://operator-7f2a:${encodeURIComponent(`${password}/x`)}@listener.example`
    );
    const basic = btoa(`operator-7f2a:${password}/x`);
    expect(scrub(`you sent Basic ${basic}`)).toBe(`you sent Basic ${WITHHELD}`);
    expect(scrub(`password ${password}/x`)).toBe(`password ${WITHHELD}`);
    expect(scrub(`escaped ${encodeURIComponent(`${password}/x`)}`)).toBe(`escaped ${WITHHELD}`);
  });

  test("a URL with no user info, or no URL, holds nothing", () => {
    holdUrlUserinfo("https://listener.example/v1");
    holdUrlUserinfo("not a url at all");
    expect(scrub("https://listener.example/v1 and not a url at all")).toBe(
      "https://listener.example/v1 and not a url at all"
    );
  });
});
