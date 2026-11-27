import { describe, expect, test } from "bun:test";
import {
  eachChunk,
  GhRetries,
  MAX_RATE_RETRIES,
  MAX_RATE_WAIT_MS,
  type ResetIo,
  splitFrontmatter,
  type TimeLimit,
  transientReason,
  untilInstallationReset,
} from "./common";
import { graphqlArgs } from "./copies";

// Catches: a wait for a primary rate limit's reset that read `gh api rate_limit`, which names no
// owner, so the pod's gh answers it from its default owner's installation and not from the one
// that refused the call. Where the default owner's reset comes sooner, each of the waits ends
// before the refusing installation's reset, and the read fails after its last try.
describe("untilInstallationReset", () => {
  const NOW = Date.parse("2026-11-20T00:00:00Z");
  const COMMIT = "1111111111111111111111111111111111111111";
  const seconds = (ms: number) => Math.floor(ms / 1000);
  const iso = (ms: number) => new Date(ms).toISOString();
  type Answer = { readonly code: number; readonly stdout: string };
  /** A `gh api -i` answer: its status line, an X-RateLimit-Reset header when `reset` (seconds
   *  since the epoch) is given, and `body`. */
  const included = (status: string, reset: number | undefined, body: unknown) =>
    [
      `HTTP/2.0 ${status}`,
      "Content-Type: application/json; charset=utf-8",
      ...(reset === undefined ? [] : [`X-Ratelimit-Reset: ${reset}`]),
      "",
      JSON.stringify(body),
    ].join("\r\n");
  /** `gh api rate_limit`'s answer: another installation's resets, a minute and half a minute
   *  away, sooner than the refusing installation's. */
  const rateLimit: Answer = {
    code: 0,
    stdout: JSON.stringify({
      resources: { core: { reset: seconds(NOW) + 60 }, graphql: { reset: seconds(NOW) + 30 } },
    }),
  };
  /** A stand-in gh at NOW: `answer` gives its answer to each call but `rate_limit`, which gets
   *  `rateLimit`; each call kept in `calls`, each line said in `lines`. */
  const standIn = (answer: (args: readonly string[]) => Answer | undefined) => {
    const calls: string[][] = [];
    const lines: string[] = [];
    const io: ResetIo = {
      gh: async (args) => {
        calls.push([...args]);
        if (args.includes("rate_limit")) return rateLimit;
        return answer(args) ?? { code: 1, stdout: "" };
      },
      say: (line) => void lines.push(line),
      now: () => NOW,
    };
    return { io, calls, lines };
  };

  test.each([
    ["build.ts's tree read", ["api", `repos/acme/x/git/trees/${COMMIT}?recursive=1`]],
    [
      "build.ts's file read",
      ["api", "-X", "GET", "repos/acme/x/contents/a.md", "-f", `ref=${COMMIT}`],
    ],
    [
      "context.ts's file read",
      [
        ...["api", "--method", "GET", "-H", "Accept: application/vnd.github.raw"],
        `repos/acme/x/contents/a.md?ref=${COMMIT}`,
      ],
    ],
    ["copies.ts's commit read", ["api", "repos/acme/x/commits?sha=main&per_page=1"]],
  ])(
    "%s, refused for the REST limit, waits until the reset of acme's installation, from the X-RateLimit-Reset header of gh api -i repos/acme/x, which gh prints on a refused answer too",
    async (_, args) => {
      const reset = seconds(NOW) + 600;
      const { io, calls, lines } = standIn((asked) =>
        asked.join(" ") === "api -i --method GET repos/acme/x"
          ? {
              code: 1,
              stdout: included("403 Forbidden", reset, {
                message: "API rate limit exceeded for installation ID 1.",
              }),
            }
          : undefined
      );
      expect(await untilInstallationReset(args, io)).toBe(601_000);
      expect(calls).toEqual([["api", "-i", "--method", "GET", "repos/acme/x"]]);
      expect(lines).toEqual([
        `acme's REST rate limit resets at ${iso(reset * 1000)}; waiting 601 s, until ${iso(NOW + 601_000)}`,
      ]);
    }
  );

  test("a call that names an owner alone (users/acme) reads the reset of acme's first repository's listing", async () => {
    const reset = seconds(NOW) + 120;
    const { io, calls } = standIn((asked) =>
      asked.join(" ") === "api -i --method GET users/acme/repos?per_page=1"
        ? { code: 0, stdout: included("200 OK", reset, [{ full_name: "acme/x" }]) }
        : undefined
    );
    expect(await untilInstallationReset(["api", "users/acme"], io)).toBe(121_000);
    expect(calls).toEqual([["api", "-i", "--method", "GET", "users/acme/repos?per_page=1"]]);
  });

  test("a GraphQL call refused for its limit reads the rateLimit.resetAt of the installation its owner field names, with its owner and name fields", async () => {
    const resetAt = "2026-11-20T00:05:00Z";
    const { io, calls, lines } = standIn((asked) =>
      asked[2] === "graphql" && asked.includes("owner=acme") && asked.includes("name=x")
        ? {
            code: 0,
            stdout: included("200 OK", seconds(NOW) + 1, { data: { rateLimit: { resetAt } } }),
          }
        : undefined
    );
    const query =
      "query($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { id } }";
    expect(await untilInstallationReset(graphqlArgs("acme/x", query), io)).toBe(301_000);
    expect(calls.map((call) => call.slice(0, 7))).toEqual([
      ["api", "-i", "graphql", "-f", "owner=acme", "-f", "name=x"],
    ]);
    expect(calls[0]?.[8]).toContain("rateLimit { resetAt }");
    expect(lines).toEqual([
      `acme's GraphQL rate limit resets at ${iso(Date.parse(resetAt))}; waiting 301 s, until ${iso(NOW + 301_000)}`,
    ]);
  });

  test("a GraphQL read of a reset that GitHub refuses too, its limit spent, gives the reset in its X-RateLimit-Reset header", async () => {
    const reset = seconds(NOW) + 240;
    const { io } = standIn((asked) =>
      asked[2] === "graphql"
        ? {
            code: 1,
            stdout: included("200 OK", reset, {
              errors: [{ type: "RATE_LIMITED", message: "API rate limit exceeded" }],
            }),
          }
        : undefined
    );
    expect(await untilInstallationReset(graphqlArgs("acme/x", "query { x }"), io)).toBe(241_000);
  });

  test("gh repo list acme, which reads the GraphQL API, reads acme's GraphQL reset with its owner field alone", async () => {
    const resetAt = "2026-11-20T00:02:00Z";
    const { io, calls } = standIn((asked) =>
      asked[2] === "graphql" &&
      asked.includes("owner=acme") &&
      !asked.some((arg) => arg.startsWith("name="))
        ? { code: 0, stdout: included("200 OK", undefined, { data: { rateLimit: { resetAt } } }) }
        : undefined
    );
    const listing = ["repo", "list", "acme", "--limit", "2000", "--json", "nameWithOwner"];
    expect(await untilInstallationReset(listing, io)).toBe(121_000);
    expect(calls.map((call) => call.slice(0, 5))).toEqual([
      ["api", "-i", "graphql", "-f", "owner=acme"],
    ]);
    expect(calls[0]?.[6]).toContain("repositoryOwner(login: $owner)");
  });

  test.each([
    [
      "a code search",
      ["api", "-i", "-X", "GET", "search/code", "-f", "q=x org:acme", "-f", "page=1"],
    ],
    ["gh api rate_limit", ["api", "rate_limit"]],
    [
      "a GraphQL call with no owner field",
      ["api", "graphql", "-f", "query=query { viewer { login } }"],
    ],
  ])("%s names no owner: it waits the 15-minute cap, with no read", async (_, args) => {
    const { io, calls, lines } = standIn(() => undefined);
    expect(await untilInstallationReset(args, io)).toBe(MAX_RATE_WAIT_MS);
    expect(MAX_RATE_WAIT_MS).toBe(900_000);
    expect(calls).toEqual([]);
    expect(lines).toEqual([
      `the refused call names no owner in its endpoint or its owner field, so no installation's reset is read; waiting 900 s, the longest rate wait, until ${iso(NOW + 900_000)}`,
    ]);
  });

  test.each([
    [
      "a read that fails with no answer",
      { code: 1, stdout: "" },
      "could not be read (gh exited 1, giving none)",
    ],
    [
      "an answer with no reset",
      { code: 0, stdout: included("200 OK", undefined, {}) },
      "could not be read (its answer gave none)",
    ],
    [
      "a reset already past",
      { code: 0, stdout: included("200 OK", seconds(NOW) - 5, {}) },
      `resets at ${iso((seconds(NOW) - 5) * 1000)}, a time already past`,
    ],
  ])("%s waits the 15-minute cap, never a minute", async (_, answer, why) => {
    const { io, lines } = standIn(() => answer);
    expect(await untilInstallationReset(["api", "repos/acme/x/git/blobs/abc"], io)).toBe(900_000);
    expect(lines).toHaveLength(1);
    expect(lines[0]).toContain(why);
    expect(lines[0]).toEndWith(
      `; waiting 900 s, the longest rate wait, until ${iso(NOW + 900_000)}`
    );
  });

  test("a reset more than 15 minutes away is waited for 15 minutes", async () => {
    const { io } = standIn(() => ({
      code: 0,
      stdout: included("200 OK", seconds(NOW) + 3_600, {}),
    }));
    expect(await untilInstallationReset(["api", "repos/acme/x"], io)).toBe(900_000);
  });
});

describe("GhRetries", () => {
  // Catches: TRANSIENT_FAILURES' `HTTP 429`, made for the npm registry's 429, matching GitHub's
  // 429 too, so a gh call still refused after its MAX_RATE_RETRIES rate waits was sent 4 times
  // more as a transient failure.
  test("a gh call GitHub refuses with HTTP 429 on every try is sent MAX_RATE_RETRIES times again, after its rate waits, and no more; the registry's own 429 is transient", async () => {
    const refused = "gh: Too Many Requests (HTTP 429)";
    const retries = new GhRetries({ sleep: async () => {}, err: () => {} }, "copies");
    let tries = 0;
    const result = await retries.send(
      async () => {
        tries += 1;
        return { code: 1, stderr: `${refused}\n` };
      },
      { rateWait: async () => 0 }
    );
    expect(tries).toBe(1 + MAX_RATE_RETRIES);
    expect(retries.record()).toEqual({});
    expect(result.lastStderr).toBe(`${refused}\n`);
    expect(transientReason(refused)).toBeUndefined();
    expect(transientReason("HTTP 429 (too many requests)")).toBe("HTTP 429");
  });

  // Catches: a gh read whose answer gh got cut short (`unexpected end of JSON input`, gh's own
  // stderr: a recursive tree of 17,568,329 bytes, 2026-11-24, whose same call gave it whole on
  // each of 3 later tries) failed on its first try, as no transient failure, and stopped a screen.
  test("a gh call whose answer gh got cut short, unexpected end of JSON input, is sent again as a transient failure, body cut short, and its next try's answer is taken", async () => {
    const cut = "unexpected end of JSON input";
    const slept: number[] = [];
    const said: string[] = [];
    const retries = new GhRetries(
      { sleep: async (ms) => void slept.push(ms), err: (line) => void said.push(line) },
      "behaviour screen"
    );
    let tries = 0;
    const result = await retries.send(async () => {
      tries += 1;
      return tries === 1 ? { code: 1, stderr: `${cut}\n` } : { code: 0, stderr: "" };
    });
    expect({ tries, code: result.code, slept, record: retries.record() }).toEqual({
      tries: 2,
      code: 0,
      slept: [5_000],
      record: { "body cut short": 1 },
    });
    expect(said).toEqual([
      `behaviour screen: a gh call failed (${cut}); sending it again in 5 s, retry 1 of 4`,
    ]);
    expect(transientReason(cut)).toBe("body cut short");
  });

  // Catches: a gh read whose HTTP/2 stream GitHub's end reset (`stream error: stream ID 1;
  // CANCEL; received from peer`, gh's own stderr: the same recursive tree of 17,568,329 bytes,
  // 2026-11-24) failing on its first try, as no transient failure, and stopping a screen.
  test("a gh call whose answer's stream GitHub's end reset is sent again as a transient failure, stream reset, and its next try's answer is taken", async () => {
    const reset = "stream error: stream ID 1; CANCEL; received from peer";
    const slept: number[] = [];
    const said: string[] = [];
    const retries = new GhRetries(
      { sleep: async (ms) => void slept.push(ms), err: (line) => void said.push(line) },
      "behaviour screen"
    );
    let tries = 0;
    const result = await retries.send(async () => {
      tries += 1;
      return tries === 1 ? { code: 1, stderr: `${reset}\n` } : { code: 0, stderr: "" };
    });
    expect({ tries, code: result.code, slept, record: retries.record() }).toEqual({
      tries: 2,
      code: 0,
      slept: [5_000],
      record: { "stream reset": 1 },
    });
    expect(said).toEqual([
      `behaviour screen: a gh call failed (${reset}); sending it again in 5 s, retry 1 of 4`,
    ]);
    // Another stream and another code read the same.
    expect(transientReason("stream error: stream ID 7; INTERNAL_ERROR; received from peer")).toBe(
      "stream reset"
    );
  });
});

describe("eachChunk", () => {
  /** A time limit that never fires. */
  const never: TimeLimit = {
    touch() {},
    timedOut: () => false,
    clear() {},
    ended: Promise.withResolvers<void>().promise,
  };

  // Catches: each read raced against the time limit's `ended`, which stays pending while the
  // limit does not fire: every race left a reaction on it that held its chunk, so a store read
  // held all of its output (a pull page, 64 MiB and more) until it ended.
  test("lets each chunk go once take returns: of 40 chunks of 1 MiB, none of the first 35 is held at the 40th", async () => {
    const CHUNKS = 40;
    let sent = 0;
    const stream = new ReadableStream<Uint8Array>({
      pull(controller) {
        sent += 1;
        if (sent > CHUNKS) controller.close();
        else controller.enqueue(new Uint8Array(1 << 20).fill(sent));
      },
    });
    const refs: WeakRef<Uint8Array>[] = [];
    let held = -1;
    await eachChunk(stream, never, (chunk) => {
      refs.push(new WeakRef(chunk));
      if (refs.length === CHUNKS) {
        Bun.gc(true);
        held = refs.slice(0, CHUNKS - 5).filter((ref) => ref.deref() !== undefined).length;
      }
    });
    expect(refs).toHaveLength(CHUNKS);
    expect(held).toBe(0);
  });

  test("ends a pending read once the limit fires, though the stream is never closed", async () => {
    const { promise: ended, resolve: fire } = Promise.withResolvers<void>();
    const stream = new ReadableStream<Uint8Array>({
      start: (controller) => controller.enqueue(new Uint8Array([104, 105])),
    });
    const taken: number[] = [];
    // The limit fires once the first chunk is taken, while the next read waits.
    await eachChunk(stream, { ...never, ended }, (chunk) => {
      taken.push(chunk.length);
      fire();
    });
    expect(taken).toEqual([2]);
  });
});

// Catches: one split of a SKILL.md's text for both of Oh My Pi's reads where they differ: the
// skill loader's (`repair`: leading HTML comments dropped, the body that `resolveCollision`
// compares) and an autoloaded block's (no `repair`: a leading comment stays, arms.ts's
// `autoloadedText`), each as `parseFrontmatter` splits at the pinned tag.
describe("splitFrontmatter", () => {
  const TEXT = "<!-- a made-up note -->\r\n---\r\nname: alpha\r\n---\r\n\r\n# Alpha\r\n\r\n";

  test("with repair, the comments before the opening --- are dropped, CRLF read as LF, and the body trimmed", () => {
    expect(splitFrontmatter(TEXT, true)).toEqual({ block: "name: alpha", body: "# Alpha" });
  });

  test("without repair, a text that opens with a comment has no block, and its body is the whole text, untrimmed", () => {
    expect(splitFrontmatter(TEXT, false)).toEqual({
      block: undefined,
      body: TEXT.replaceAll("\r\n", "\n"),
    });
  });

  test("the rest of the closing line is the body's, and a text with no closing line has no block", () => {
    expect(splitFrontmatter("---\nname: beta\n----\nBody.", true)).toEqual({
      block: "name: beta",
      body: "-\nBody.",
    });
    expect(splitFrontmatter("---\nname: beta\n", true)).toEqual({
      block: undefined,
      body: "---\nname: beta\n",
    });
  });
});
