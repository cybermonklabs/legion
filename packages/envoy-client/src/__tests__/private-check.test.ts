// The Dispatch client returns nothing to a bearer that names a private project, and asks nothing
// extra while no project is private (src/private-check.ts). Each row drives `DispatchClient`, as
// `checkedDispatchClient` makes it, against a stand-in Dispatch that answers the header each row
// names, and counts what the client sent. The state each row starts from is a cache directory of
// its own and a Dispatch URL of its own, so no row reads another's.
import { describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { bearerCredential, type RequestCredential } from "../bearer";
import { runDispatchCli } from "../dispatch-cli";
import {
  checkedDispatchClient,
  DISPATCH_WRITE_RETRY,
  DispatchClient,
  DispatchServiceError,
} from "../dispatch-http";
import {
  checkedDispatchFetch,
  PRIVATE_PROJECTS_HEADER,
  PrivateDispatchUnanswered,
  PrivateProjectRefused,
} from "../private-check";

const makeDirectory = temporaryDirectories("private-check");

/** A personal token whose whoami names the private project PRIV, and one that names none. */
const PRIV_TOKEN = `dsp_${fixtureSecret("priv")}`;
const PUB_TOKEN = `dsp_${fixtureSecret("pub")}`;
/** A broker credential: a DPoP-bound token and its proof, which the check admits unasked. */
const BROKER: RequestCredential = {
  scheme: "DPoP",
  headers: () => ({ Authorization: "DPoP broker-access-token", DPoP: "proof" }),
  secret: () => "broker-access-token",
};
/** A credential that presents nothing: the request goes with no credential. */
const NONE: RequestCredential = { scheme: "Bearer", headers: () => ({}), secret: () => "" };

/** Words only PRIV-1's answer holds, which no refusal may carry to the caller. */
const PRIVATE_TEXT = "the private project's own words";

const MINUTE = 60_000;

interface Sent {
  readonly method: string;
  readonly path: string;
  readonly authorization: string | null;
}

type Header = "0" | "1" | undefined;

/**
 * A stand-in Dispatch. `header` is what each answer carries in `Dispatch-Private-Projects`
 * (undefined: none), read at each request so a row can change it between calls; `whoami` answers
 * `GET /api/v1/whoami` for a bearer, and `answer` every other request (PRIV-1's issue, a write's
 * echo of its key, by default). `down` makes every request fail in transport.
 */
function standIn(
  options: {
    header?: () => Header;
    whoami?: (bearer: string) => Response;
    answer?: (request: Request) => Response;
  } = {}
) {
  const sent: Sent[] = [];
  let down = false;
  const fetchImpl = (async (input: string | URL | Request, init?: RequestInit) => {
    const request = new Request(input, init);
    const { pathname } = new URL(request.url);
    const authorization = request.headers.get("Authorization");
    sent.push({ method: request.method, path: pathname, authorization });
    if (down) throw new TypeError("fetch failed");
    let response: Response;
    if (pathname === "/api/v1/whoami") {
      const bearer = authorization?.replace(/^Bearer /, "") ?? "";
      response = (options.whoami ?? whoamiNaming)(bearer);
    } else if (options.answer !== undefined) {
      response = options.answer(request);
    } else if (request.method === "GET") {
      response = Response.json({ key: "PRIV-1", title: PRIVATE_TEXT, last_seq: 0 });
    } else {
      response = Response.json(
        { key: "PRIV-1", title: PRIVATE_TEXT },
        { headers: { "Idempotency-Key": request.headers.get("Idempotency-Key") ?? "" } }
      );
    }
    const header = options.header?.();
    if (header !== undefined) response.headers.set(PRIVATE_PROJECTS_HEADER, header);
    return response;
  }) as typeof fetch;
  return {
    fetchImpl,
    sent,
    whoamis: () => sent.filter(({ path }) => path === "/api/v1/whoami"),
    writes: () => sent.filter(({ method }) => method !== "GET"),
    goDown: () => {
      down = true;
    },
  };
}

/** Today's whoami of a personal token: PRIV's names PRIV as private, any other names none. */
function whoamiNaming(bearer: string): Response {
  return Response.json({
    kind: "agent",
    owner: "person@example.com",
    projects: ["PRIV", "PUB"],
    ...(bearer === PRIV_TOKEN ? { private_projects: ["PRIV"] } : {}),
  });
}

let made = 0;

/** A Dispatch URL, a cache directory and a clock of the row's own, with the clients it makes. */
function rig(fetchImpl: typeof fetch, cache = makeDirectory("cache")) {
  made += 1;
  const url = `http://dispatch-${made}.test`;
  let now = Date.parse("2026-11-26T12:00:00Z");
  const warnings: string[] = [];
  const settings = {
    env: { XDG_CACHE_HOME: cache },
    now: () => now,
    warn: (line: string) => {
      warnings.push(line);
    },
  };
  const writeRetry = { ...DISPATCH_WRITE_RETRY, pause: async () => {} };
  return {
    url,
    cache,
    warnings,
    settings,
    client: (credential: RequestCredential) =>
      checkedDispatchClient(url, credential, fetchImpl, { privateCheck: settings, writeRetry }),
    unchecked: (credential: RequestCredential) =>
      new DispatchClient(url, credential, fetchImpl, { writeRetry }),
    advance: (ms: number) => {
      now += ms;
    },
  };
}

/** What a call threw, or a failure naming what it answered instead. */
async function thrown(call: Promise<unknown>): Promise<unknown> {
  try {
    const answer = await call;
    throw new Error(`the call answered ${JSON.stringify(answer)} rather than throwing`);
  } catch (error) {
    return error;
  }
}

describe("a Dispatch that does not say whether a project is private", () => {
  test("its answer comes back once whoami answers 404, with one warning; after a 1 that 404 is no answer", async () => {
    let header: Header;
    const dispatch = standIn({
      header: () => header,
      whoami: () => Response.json({ error: "no route", code: "NOT_FOUND" }, { status: 404 }),
    });
    const { client, warnings, url } = rig(dispatch.fetchImpl);

    expect((await client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1")).title).toBe(
      PRIVATE_TEXT
    );
    expect(await client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1")).toBeDefined();
    expect(dispatch.whoamis()).toHaveLength(1);
    expect(warnings).toHaveLength(1);
    expect(warnings[0]).toContain(`GET ${url}/api/v1/whoami answered 404`);

    header = "1";
    const error = await thrown(client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"));
    expect(error).toBeInstanceOf(PrivateDispatchUnanswered);
    expect(String(error)).not.toContain(PRIVATE_TEXT);
  });

  test("a header-less 2xx under no trusted 0 is held, and a bearer naming a private project gets nothing", async () => {
    const dispatch = standIn();
    const { client } = rig(dispatch.fetchImpl);
    const error = await thrown(client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"));
    expect(error).toBeInstanceOf(PrivateProjectRefused);
    expect(String(error)).toContain(
      "private_project_refused: PRIV is private; its sessions run only in its own cluster namespace"
    );
    expect(String(error)).not.toContain(PRIVATE_TEXT);
  });

  test("a header-less non-2xx holds no object of Dispatch's and comes back as it came, with no whoami", async () => {
    const dispatch = standIn({
      answer: () => new Response("<html>502 Bad Gateway</html>", { status: 502 }),
    });
    const { client, unchecked } = rig(dispatch.fetchImpl);
    const checked = await thrown(client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"));
    const today = await thrown(unchecked(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"));
    expect(checked).toBeInstanceOf(DispatchServiceError);
    expect(String(checked)).toBe(String(today));
    expect(dispatch.whoamis()).toHaveLength(0);
  });
});

describe("a Dispatch that answers 0", () => {
  test("a read comes back as it came and no whoami is asked, even while whoami answers 503", async () => {
    const dispatch = standIn({
      header: () => "0",
      whoami: () => Response.json({ error: "down" }, { status: 503 }),
    });
    const { client } = rig(dispatch.fetchImpl);
    expect((await client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1")).title).toBe(
      PRIVATE_TEXT
    );
    expect(dispatch.whoamis()).toHaveLength(0);
  });

  test("a trusted 0 lapses: 4 minutes on a header-less 2xx comes back, 6 minutes on it is held and checked", async () => {
    let header: Header = "0";
    const dispatch = standIn({ header: () => header });
    const { client, advance } = rig(dispatch.fetchImpl);
    await client(bearerCredential(PUB_TOKEN)).getIssue("PRIV-1");
    header = undefined;
    advance(4 * MINUTE);
    expect((await client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1")).title).toBe(
      PRIVATE_TEXT
    );
    expect(dispatch.whoamis()).toHaveLength(0);
    advance(2 * MINUTE);
    expect(await thrown(client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"))).toBeInstanceOf(
      PrivateProjectRefused
    );
    expect(dispatch.whoamis()).toHaveLength(1);
  });

  test("a write answered 1 from a state of 0 is held, and its answer is dropped when whoami names PRIV", async () => {
    let header: Header = "0";
    const dispatch = standIn({ header: () => header });
    const { client } = rig(dispatch.fetchImpl);
    await client(bearerCredential(PUB_TOKEN)).getIssue("PRIV-1");
    header = "1";
    const error = await thrown(
      client(bearerCredential(PRIV_TOKEN)).updateIssue("PRIV-1", { title: "new" })
    );
    expect(error).toBeInstanceOf(PrivateProjectRefused);
    expect(String(error)).not.toContain(PRIVATE_TEXT);
    // Nothing was asked first: the write went, and its answer said 1.
    expect(dispatch.sent.map(({ method, path }) => `${method} ${path}`)).toEqual([
      "GET /api/v1/issues/PRIV-1",
      "PATCH /api/v1/issues/PRIV-1",
      "GET /api/v1/whoami",
    ]);
  });
});

describe("a Dispatch that has answered 1", () => {
  test("a 1 sticks until Dispatch's own 0 has held for 6 minutes", async () => {
    let header: Header = "1";
    const dispatch = standIn({ header: () => header });
    const { client, advance } = rig(dispatch.fetchImpl);
    await client(bearerCredential(PUB_TOKEN)).getIssue("PRIV-1");
    // Dispatch's own 0 from minute 1: held 5 minutes by minute 6, 7 minutes by minute 8.
    header = "0";
    for (let minute = 1; minute <= 6; minute += 1) {
      advance(MINUTE);
      await client(BROKER).getIssue("PRIV-1");
    }
    // Five minutes of 0, and the PRIV read is still checked beside its request, and dropped.
    expect(await thrown(client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"))).toBeInstanceOf(
      PrivateProjectRefused
    );
    for (let minute = 7; minute <= 8; minute += 1) {
      advance(MINUTE);
      await client(BROKER).getIssue("PRIV-1");
    }
    // Seven minutes of 0: the state is 0, nothing is asked first, and the answer's own 0 returns it.
    const whoamis = dispatch.whoamis().length;
    expect((await client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1")).title).toBe(
      PRIVATE_TEXT
    );
    expect(dispatch.whoamis()).toHaveLength(whoamis);
  });

  test("a header-less answer never reads as 0: after a 1, a 0 and 7 minutes, a 502, then a PRIV write is not sent", async () => {
    let header: Header = "1";
    let gateway = false;
    const dispatch = standIn({
      header: () => header,
      answer: (request) =>
        gateway
          ? new Response("<html>502 Bad Gateway</html>", { status: 502 })
          : request.method === "GET"
            ? Response.json({ key: "PRIV-1", title: PRIVATE_TEXT })
            : Response.json({ key: "PRIV-1" }),
    });
    const { client, advance } = rig(dispatch.fetchImpl);
    await client(BROKER).getIssue("PRIV-1");
    header = "0";
    advance(MINUTE);
    await client(BROKER).getIssue("PRIV-1");
    advance(7 * MINUTE);
    header = undefined;
    gateway = true;
    expect(await thrown(client(BROKER).getIssue("PRIV-1"))).toBeInstanceOf(DispatchServiceError);
    gateway = false;
    const error = await thrown(
      client(bearerCredential(PRIV_TOKEN)).updateIssue("PRIV-1", { title: "new" })
    );
    expect(error).toBeInstanceOf(PrivateProjectRefused);
    expect(dispatch.whoamis()).toHaveLength(1);
    expect(dispatch.writes()).toHaveLength(0);
  });

  test("a bearer naming PRIV is refused and its read's answer never reaches the caller, whatever the environment says", async () => {
    const dispatch = standIn({ header: () => "1" });
    const { client, settings } = rig(dispatch.fetchImpl);
    expect(await thrown(client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"))).toBeInstanceOf(
      PrivateProjectRefused
    );
    // No variable admits a key: the refusal is the bearer's, wherever it runs.
    const before = process.env.AGENT_PRIVATE_SESSION;
    process.env.AGENT_PRIVATE_SESSION = "1";
    try {
      const admitted = checkedDispatchClient(
        `http://dispatch-${made}.test`,
        bearerCredential(PRIV_TOKEN),
        dispatch.fetchImpl,
        { privateCheck: { ...settings, env: { ...settings.env, AGENT_PRIVATE_SESSION: "1" } } }
      );
      const error = await thrown(admitted.getIssue("PRIV-1"));
      expect(error).toBeInstanceOf(PrivateProjectRefused);
      expect(String(error)).not.toContain(PRIVATE_TEXT);
    } finally {
      if (before === undefined) delete process.env.AGENT_PRIVATE_SESSION;
      else process.env.AGENT_PRIVATE_SESSION = before;
    }
  });

  test("a write with a bearer waits for the check, so a refused bearer writes nothing", async () => {
    const dispatch = standIn({ header: () => "1" });
    const { client } = rig(dispatch.fetchImpl);
    await client(BROKER).getIssue("PRIV-1");
    expect(
      await thrown(client(bearerCredential(PRIV_TOKEN)).updateIssue("PRIV-1", { title: "new" }))
    ).toBeInstanceOf(PrivateProjectRefused);
    expect(dispatch.writes()).toHaveLength(0);
    expect(dispatch.whoamis()).toHaveLength(1);
  });

  test("a broker credential is admitted without whoami", async () => {
    const dispatch = standIn({ header: () => "1" });
    const { client } = rig(dispatch.fetchImpl);
    expect((await client(BROKER).getIssue("PRIV-1")).title).toBe(PRIVATE_TEXT);
    await client(BROKER).updateIssue("PRIV-1", { title: "new" });
    expect(dispatch.whoamis()).toHaveLength(0);
    expect(dispatch.writes()).toHaveLength(1);
  });

  test("a bearer naming nothing private is admitted, and asked again only after 5 minutes", async () => {
    const dispatch = standIn({ header: () => "1" });
    const { client, advance } = rig(dispatch.fetchImpl);
    expect((await client(bearerCredential(PUB_TOKEN)).getIssue("PRIV-1")).title).toBe(PRIVATE_TEXT);
    await client(bearerCredential(PUB_TOKEN)).updateIssue("PRIV-1", { title: "new" });
    advance(4 * MINUTE);
    await client(bearerCredential(PUB_TOKEN)).getIssue("PRIV-1");
    expect(dispatch.whoamis()).toHaveLength(1);
    advance(2 * MINUTE);
    await client(bearerCredential(PUB_TOKEN)).getIssue("PRIV-1");
    expect(dispatch.whoamis()).toHaveLength(2);
  });

  test("a refusal is remembered for 24 hours", async () => {
    const dispatch = standIn({ header: () => "1" });
    const { client, advance } = rig(dispatch.fetchImpl);
    await thrown(client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"));
    advance(23 * 60 * MINUTE);
    await thrown(client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"));
    expect(dispatch.whoamis()).toHaveLength(1);
    advance(2 * 60 * MINUTE);
    await thrown(client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"));
    expect(dispatch.whoamis()).toHaveLength(2);
  });

  test("a whoami that answers 503 through the retry window fails closed", async () => {
    const dispatch = standIn({
      header: () => "1",
      whoami: () => Response.json({ error: "unavailable" }, { status: 503 }),
    });
    const { client } = rig(dispatch.fetchImpl);
    const error = await thrown(client(bearerCredential(PUB_TOKEN)).getIssue("PRIV-1"));
    expect(error).toBeInstanceOf(PrivateDispatchUnanswered);
    expect(String(error)).toContain("Dispatch did not answer; retry.");
    expect(String(error)).not.toContain(PRIVATE_TEXT);
    // The first ask and one after each of the write policy's four pauses.
    expect(dispatch.whoamis()).toHaveLength(DISPATCH_WRITE_RETRY.pausesMs.length + 1);
  });

  test("a whoami answered 401 is the request's own answer", async () => {
    const dispatch = standIn({
      header: () => "1",
      whoami: () => Response.json({ error: "revoked", code: "TOKEN_REVOKED" }, { status: 401 }),
    });
    const { client } = rig(dispatch.fetchImpl);
    const error = await thrown(client(bearerCredential(PUB_TOKEN)).getIssue("PRIV-1"));
    expect(error).toBeInstanceOf(DispatchServiceError);
    expect((error as DispatchServiceError).status).toBe(401);
    expect((error as DispatchServiceError).code).toBe("TOKEN_REVOKED");
    expect(String(error)).not.toContain(PRIVATE_TEXT);
  });

  test("a request with no credential makes no whoami, and its 401 is the same typed refusal as today", async () => {
    const dispatch = standIn({
      header: () => "1",
      answer: () =>
        Response.json({ error: "sign in", code: "AUTHENTICATION_REQUIRED" }, { status: 401 }),
    });
    const { client, unchecked } = rig(dispatch.fetchImpl);
    const checked = await thrown(client(NONE).getIssue("PRIV-1"));
    const today = await thrown(unchecked(NONE).getIssue("PRIV-1"));
    expect(checked).toBeInstanceOf(DispatchServiceError);
    expect((checked as DispatchServiceError).code).toBe("AUTHENTICATION_REQUIRED");
    expect(String(checked)).toBe(String(today));
    expect(dispatch.whoamis()).toHaveLength(0);
  });
});

describe("the shared state", () => {
  test("another process of the user reads the 1 this one recorded", async () => {
    const dispatch = standIn({ header: () => "1" });
    const { client, url, cache } = rig(dispatch.fetchImpl);
    await client(BROKER).getIssue("PRIV-1");
    const child = Bun.spawn(
      [
        process.execPath,
        "-e",
        `import { PrivateCheck } from ${JSON.stringify(join(import.meta.dir, "../private-check.ts"))};
         const check = new PrivateCheck(${JSON.stringify(url)}, fetch, { env: { XDG_CACHE_HOME: ${JSON.stringify(cache)} } });
         process.stdout.write(await check.state());`,
      ],
      { stdout: "pipe", stderr: "pipe" }
    );
    const [code, stdout, stderr] = await Promise.all([
      child.exited,
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
    ]);
    expect({ code, stdout, stderr }).toEqual({ code: 0, stdout: "1", stderr: "" });
  });

  test("a cache directory nobody can write changes nothing but the sharing", async () => {
    // A regular file where the cache directory would be: no process, root included, can make a
    // directory under it, which a directory's mode bits would not stop root from doing.
    const cache = join(makeDirectory("not-a-directory"), "cache");
    writeFileSync(cache, "a file, not a directory\n");
    const dispatch = standIn({ header: () => "1" });
    const { client } = rig(dispatch.fetchImpl, cache);
    expect((await client(BROKER).getIssue("PRIV-1")).title).toBe(PRIVATE_TEXT);
    expect(await thrown(client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"))).toBeInstanceOf(
      PrivateProjectRefused
    );
    expect(readFileSync(cache, "utf8")).toBe("a file, not a directory\n");
  });

  test("the state file holds times and no bearer", async () => {
    const dispatch = standIn({ header: () => "1" });
    const { client, cache } = rig(dispatch.fetchImpl);
    await thrown(client(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1"));
    const dir = join(cache, "agent-private", "state");
    const files = new Bun.Glob("*").scanSync(dir);
    const [name] = [...files];
    expect(name).toMatch(/^[0-9a-f]{64}$/);
    const text = readFileSync(join(dir, name ?? ""), "utf8");
    expect(Object.keys(JSON.parse(text))).toEqual(["one_at"]);
    expect(text).not.toContain(PRIV_TOKEN.slice(4));
  });
});

describe("with no project private, the requests are today's", () => {
  /** A read, a write and a read of asks, as one command makes them. */
  async function command(client: DispatchClient): Promise<string[]> {
    const outcomes: string[] = [];
    for (const call of [
      () => client.getIssue("PRIV-1"),
      () => client.updateIssue("PRIV-1", { title: "new" }),
      () => client.listIssueAsks("PRIV-1"),
    ]) {
      try {
        await call();
        outcomes.push("answered");
      } catch (error) {
        outcomes.push(String(error));
      }
    }
    return outcomes;
  }

  for (const up of [true, false]) {
    test(`against a Dispatch answering 0 and ${up ? "up" : "down"}, the checked client sends exactly what the unchecked one does`, async () => {
      const today = standIn({ header: () => "0" });
      const checked = standIn({ header: () => "0" });
      if (!up) {
        today.goDown();
        checked.goDown();
      }
      const before = await command(rig(today.fetchImpl).unchecked(bearerCredential(PRIV_TOKEN)));
      const after = await command(rig(checked.fetchImpl).client(bearerCredential(PRIV_TOKEN)));
      const named = (outcomes: string[]) =>
        outcomes.map((outcome) => outcome.replaceAll(/dispatch-\d+\.test/g, "dispatch.test"));
      expect(named(after)).toEqual(named(before));
      expect(checked.sent.map(({ method, path }) => `${method} ${path}`)).toEqual(
        today.sent.map(({ method, path }) => `${method} ${path}`)
      );
      expect(checked.whoamis()).toHaveLength(0);
    });
  }

  test("a client made without the check behaves as today, whatever Dispatch answers", async () => {
    const dispatch = standIn({ header: () => "1" });
    const { unchecked } = rig(dispatch.fetchImpl);
    expect((await unchecked(bearerCredential(PRIV_TOKEN)).getIssue("PRIV-1")).title).toBe(
      PRIVATE_TEXT
    );
    expect(dispatch.whoamis()).toHaveLength(0);
  });
});

describe("a request a caller sends itself (checkedDispatchFetch)", () => {
  test("goes as written under a 0, and is refused under a 1 for a bearer naming PRIV", async () => {
    let header: Header = "0";
    const dispatch = standIn({ header: () => header });
    const { url, settings } = rig(dispatch.fetchImpl);
    const send = checkedDispatchFetch(url, dispatch.fetchImpl, settings);
    const read = { headers: { Authorization: `Bearer ${PRIV_TOKEN}` } };
    expect((await send(`${url}/api/v1/issues/PRIV-1`, read)).status).toBe(200);
    expect(dispatch.whoamis()).toHaveLength(0);
    header = "1";
    expect(await thrown(send(`${url}/api/v1/issues/PRIV-1`, read))).toBeInstanceOf(
      PrivateProjectRefused
    );
  });
});

describe("the dispatch command", () => {
  test("exits 2 naming the refusal, and prints nothing of the answer", async () => {
    const dispatch = standIn({ header: () => "1" });
    const home = makeDirectory("home");
    let stdout = "";
    let stderr = "";
    const code = await runDispatchCli(
      ["read", "--issue", "PRIV-1"],
      {
        HOME: home,
        DISPATCH_STATE_DIR: home,
        DISPATCH_URL: `http://dispatch-cli-${made}.test`,
        DISPATCH_TOKEN: PRIV_TOKEN,
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: "ses-private-check",
      },
      {
        stdout: (text) => {
          stdout += text;
        },
        stderr: (text) => {
          stderr += text;
        },
        readText: () => "",
        cwd: home,
        fetchImpl: dispatch.fetchImpl,
      }
    );
    expect({ code, stdout: stdout.trim() }).toEqual({
      code: 2,
      stdout:
        "private_project_refused: PRIV is private; its sessions run only in its own cluster namespace",
    });
    expect(stdout + stderr).not.toContain(PRIVATE_TEXT);
  });
});
