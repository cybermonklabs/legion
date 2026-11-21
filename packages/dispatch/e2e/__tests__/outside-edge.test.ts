/** The outside driver, `e2e/acceptance/outside-edge.ts`: what works for a person from outside,
 *  through the edge that serves them. Its step 4's rule is `outside-edge-limits.test.ts`'s.
 *
 *  The rows that start nothing hold that a run with an input missing says UNKNOWN, naming the
 *  input, and never PASS. The control rows run the driver against an outside origin stand-in
 *  (`outside-edge-origin-stand-in.ts`) that answers what a real refusal or revocation would not:
 *  - `t5`'s passkey row is PASS only on the sign-in's own refusal of the passkey, 401
 *    PASSKEY_UNKNOWN or 403 NOT_LISTED from `POST /auth/passkey/login/finish`; an alert with no
 *    answer ("Dispatch could not be reached"), a 404, a 429, any other code is UNKNOWN, and a
 *    2xx is FAIL;
 *  - `revoke` passes a credential only when the list, read again, no longer holds it, and a list
 *    that gives no answer is UNKNOWN;
 *  - the link's token reaches no row, stdout, stderr, TSV or state file, also where the page did
 *    not load or names the link itself;
 *  - the request counter sees the relay socket the driver's relay client opens.
 *  The rest read the runs of `outside-edge-stack.ts`: one stack of the e2e harness (whose server
 *  `e2e/run-server.sh` starts under `DISPATCH_E2E_SIGNIN=google`, with passkey sign-in on at the
 *  outside origin), the edge stand-in of `outside-edge-stand-in.ts`, and the driver run against
 *  them once per case, each case a person of its own:
 *  - a person on one project passes every step, approves the tailnet leg's code beside its own,
 *    and `approve` approves one more code;
 *  - a person on a second project fails step 1, naming it (the project-leak control);
 *  - a peer that never answers fails step 3 by its timeout, naming the peer;
 *  - a threshold of 1 fails step 4, naming the host;
 *  - `revoke` reads the person's own machine-logins list, and ends each credential there;
 *  - `t5` reads UNKNOWN while the person is listed, PASS once the owner took them off, and FAIL
 *    for a person still listed whose cookie is refused: their restored passkey signs them in,
 *    which is what shows the passkey `t5` tries is one Dispatch would take.
 *
 *  WHY THE STACK ROWS ARE SKIPPED WITHOUT A POSTGRES PROVIDER. The stack is a Postgres, a Go build
 *  of the server and the fakes, and needs `DISPATCH_E2E_PG_BIN` or `DISPATCH_E2E_PG_IMAGE` to make
 *  its database (`e2e/harness-postgres.ts`); without one they prove nothing, so they are skipped
 *  and say so. They also need Chromium and the built dashboard (`bun run build:web`), which the
 *  child refuses to run without. CI runs them in `.github/workflows/dispatch-browser.yaml`'s
 *  two-process job, which holds all three. The control rows that start Chromium skip where
 *  Playwright's Chromium is not installed, as in `ci.yaml`'s `dispatch-ui` job, and run in that
 *  same two-process job. */

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { generateKeyPairSync, randomBytes } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "@playwright/test";
import { type BoundName, rowLimit, runBounded } from "./bounded-subprocess";
import {
  type OriginBehaviour,
  startOriginStandIn,
  unreachedAlert,
} from "./outside-edge-origin-stand-in";
import { type DriverRun, peer, people, type StackRuns, silentPeer } from "./outside-edge-stack";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const driver = join(dispatchPackage, "e2e/acceptance/outside-edge.ts");
const stackProgram = fileURLToPath(new URL("./outside-edge-stack.ts", import.meta.url));

/** Runs the driver with `args` and these variables alone, beside PATH, HOME and where
 *  Playwright's browsers are, stopped at `bound`. */
function drive(
  args: readonly string[],
  env: Record<string, string>,
  bound: BoundName
): Promise<DriverRun> {
  return runBounded({
    argv: ["bun", driver, ...args],
    bound,
    cwd: dispatchPackage,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: process.env.HOME ?? "",
      ...(process.env.PLAYWRIGHT_BROWSERS_PATH === undefined
        ? {}
        : { PLAYWRIGHT_BROWSERS_PATH: process.env.PLAYWRIGHT_BROWSERS_PATH }),
      ...env,
    },
    from: import.meta.url,
  });
}

/** The TSV rows a run printed, by name: `row`, `want`, `got`, verdict. */
function rowsOf(run: DriverRun): Map<string, { want: string; got: string; verdict: string }> {
  const rows = new Map<string, { want: string; got: string; verdict: string }>();
  for (const line of run.stdout.split("\n")) {
    const [row, want, got, verdict, ...rest] = line.split("\t");
    if (row === undefined || verdict === undefined || rest.length > 0) continue;
    rows.set(row, { want: want ?? "", got: got ?? "", verdict });
  }
  return rows;
}

/** The verdict line: the run's last line. */
function verdictLine(run: DriverRun): string {
  return run.stdout.trimEnd().split("\n").at(-1) ?? "";
}

describe("the inputs", () => {
  const scratch = mkdtempSync(join(tmpdir(), "outside-edge-inputs-"));
  afterAll(() => rmSync(scratch, { force: true, recursive: true }));
  const limits = join(scratch, "limits.txt");
  writeFileSync(limits, "3010 dispatch.edge.example any - 1200 60\n");
  // In the order the driver reads them, so a run missing one is refused for that one first.
  const all = {
    OUTSIDE_INVITE_URL: "https://dispatch.edge.example/enroll#a-token",
    OUTSIDE_PROJECT: "OWN",
    OUTSIDE_PEER_SESSION: peer,
    OUTSIDE_BROKER_URL: "https://secrets.edge.example",
    OUTSIDE_LISTENER_URL: "https://envoy.edge.example",
    OUTSIDE_EDGE_LIMITS: limits,
    OUTSIDE_EDGE_STATE: join(scratch, "state"),
  } as const;

  for (const name of Object.keys(all) as (keyof typeof all)[]) {
    test(
      `a run without ${name} is UNKNOWN, naming it, and starts nothing`,
      async () => {
        const run = await drive(
          [],
          Object.fromEntries(Object.entries(all).filter(([key]) => key !== name)),
          "short"
        );
        expect(run.stdout).toBe(
          `inputs\tevery input of the driver\t${name} is not set\tUNKNOWN\nUNKNOWN: inputs\n`
        );
        expect(run.code).toBe(2);
      },
      rowLimit("short")
    );
  }

  test(
    "an http origin is refused without OUTSIDE_EDGE_ALLOW_HTTP, and an unreadable limits file is UNKNOWN",
    async () => {
      const http = await drive(
        [],
        { ...all, OUTSIDE_INVITE_URL: "http://127.0.0.1:9/enroll#a-token" },
        "short"
      );
      expect(http.stdout).toContain("\tOUTSIDE_INVITE_URL must be an https URL\tUNKNOWN\n");
      expect(http.code).toBe(2);
      const unread = await drive(
        [],
        { ...all, OUTSIDE_EDGE_LIMITS: join(scratch, "no-such-file") },
        "short"
      );
      expect(unread.stdout).toContain("\tOUTSIDE_EDGE_LIMITS could not be read: ");
      expect(unread.code).toBe(2);
    },
    rowLimit("short", "short")
  );
});

/** Whether Playwright's Chromium is installed here: the control rows that start it skip where it
 *  is not, as in `ci.yaml`'s `dispatch-ui` job, which installs no browser. */
const chromiumHere = existsSync(chromium.executablePath());
const browserRow = chromiumHere ? test : test.skip;
if (!chromiumHere) {
  console.log(
    "outside-edge.test.ts: the control rows that start Chromium are skipped: Playwright's Chromium is not installed here"
  );
}

describe("the controls, against an outside origin stand-in", () => {
  const scratch = mkdtempSync(join(tmpdir(), "outside-edge-controls-"));
  afterAll(() => rmSync(scratch, { force: true, recursive: true }));
  let made = 0;
  /** A state directory as `run` makes one, mode 0700, holding `files`, each mode 0600. */
  const stateWith = (files: Record<string, string>): string => {
    made += 1;
    const dir = join(scratch, `state-${made}`);
    mkdirSync(dir, { mode: 0o700 });
    for (const [name, text] of Object.entries(files)) {
      writeFileSync(join(dir, name), text, { mode: 0o600 });
    }
    return dir;
  };
  /** What `revoke` reads: a run's kept state for the stand-in at `origin`, one approved login. */
  const revokeState = (origin: string): string =>
    stateWith({
      origin,
      cookie: "dsession=stand-in",
      person: "carol@outside.example",
      "approved.tsv": "ABCD-EFGH\tcred-a\n",
    });

  test(
    "revoke passes a credential only when the list, read again, no longer holds it",
    async () => {
      const ends = startOriginStandIn({ listed: ["cred-a"], revokeRemoves: true });
      // The control: a revoke that answers 200 and ends nothing.
      const keeps = startOriginStandIn({ listed: ["cred-a"], revokeRemoves: false });
      try {
        const [ended, kept] = await Promise.all([
          drive(
            ["revoke"],
            { DISPATCH_E2E_CASE: "revoke-ends", OUTSIDE_EDGE_STATE: revokeState(ends.url) },
            "short"
          ),
          drive(
            ["revoke"],
            { DISPATCH_E2E_CASE: "revoke-keeps", OUTSIDE_EDGE_STATE: revokeState(keeps.url) },
            "short"
          ),
        ]);
        expect(rowsOf(ended).get("revoke-cred-a"), ended.stdout).toEqual({
          want: "the login of ABCD-EFGH is on the person's machine-logins list and is revoked there",
          got: "listed; the revoke answered 200, and the list read again no longer holds it",
          verdict: "PASS",
        });
        expect(ended.code).toBe(0);
        expect(rowsOf(kept).get("revoke-cred-a"), kept.stdout).toMatchObject({
          got: "listed; the revoke answered 200, and the list read again still holds it",
          verdict: "FAIL",
        });
        expect(kept.code).toBe(1);
        expect([ends.revoked, keeps.revoked]).toEqual([["cred-a"], ["cred-a"]]);
      } finally {
        ends.stop();
        keeps.stop();
      }
    },
    rowLimit({ atOnce: ["short", "short"] })
  );

  test(
    "revoke reads UNKNOWN, not FAIL, when the list gives no answer",
    async () => {
      // A port nothing listens on: the one a server was given, read after it stopped.
      const gone = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch: () => new Response() });
      const origin = `http://127.0.0.1:${gone.port}`;
      gone.stop(true);
      const run = await drive(["revoke"], { OUTSIDE_EDGE_STATE: revokeState(origin) }, "short");
      const row = rowsOf(run).get("revoke-cred-a");
      expect(row?.got, run.stdout).toStartWith("GET /api/v1/machine-logins got no answer: ");
      expect(row?.verdict).toBe("UNKNOWN");
      expect(verdictLine(run)).toBe("UNKNOWN: revoke-cred-a");
      expect(run.code).toBe(2);
    },
    rowLimit("short")
  );

  test(
    "the request counter sees the relay socket the driver's relay client opens",
    async () => {
      // The order of loading is what this row reads, so the script imports at run time: the
      // driver's module first, which loads its own imports in its own order, and then the relay
      // client that order loaded, which opens a socket at a local server that takes the upgrade.
      const script = [
        `await import(${JSON.stringify(driver)});`,
        `const { counted } = await import(${JSON.stringify(join(dispatchPackage, "e2e/acceptance/outside-edge-count.ts"))});`,
        `const { connectListenerRelay } = await import("@legion/envoy-client/listener-relay");`,
        "const server = Bun.serve({",
        '  hostname: "127.0.0.1",',
        "  port: 0,",
        "  fetch(request, at) {",
        '    if (at.upgrade(request, { headers: { "Sec-WebSocket-Protocol": "envoy-relay.v1" } })) return;',
        '    return new Response("not here", { status: 404 });',
        "  },",
        "  websocket: { message() {} },",
        "});",
        "const credential = { headers: async () => ({}), stale: () => undefined };",
        "try {",
        "  const relay = await connectListenerRelay(",
        '    { url: "http://127.0.0.1:" + server.port, session: "outside-e2e-count", credential,',
        "      outage: { refused() {}, accepted() {} }, log() {} },",
        "    AbortSignal.timeout(5_000)",
        "  );",
        "  await relay.close();",
        "} catch (error) {",
        '  console.log("open: " + (error instanceof Error ? error.message : String(error)));',
        "}",
        "const { port } = server;",
        "server.stop(true);",
        "console.log(JSON.stringify({ port, counted: counted.map((one) => one.url) }));",
        "process.exit(0);",
      ].join("\n");
      const run = await runBounded({
        argv: ["bun", "--eval", script],
        bound: "short",
        cwd: dispatchPackage,
        env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "" },
        from: import.meta.url,
      });
      expect(run.code, `${run.stdout}\n${run.stderr}`).toBe(0);
      const { port, counted } = JSON.parse(run.stdout.trimEnd().split("\n").at(-1) ?? "{}") as {
        port: number;
        counted: string[];
      };
      expect(counted, run.stdout).toContain(
        `ws://127.0.0.1:${port}/v1/sessions/outside-e2e-count/relay`
      );
    },
    rowLimit("short")
  );

  /** A passkey record as `run` keeps one: a real P-256 key in PKCS#8, as WebAuthn.addCredential
   *  takes it, for a credential the stand-in never reads. */
  const passkeyRecord = (): string => {
    const { privateKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
    return JSON.stringify({
      credentialId: randomBytes(16).toString("base64"),
      isResidentCredential: true,
      rpId: "127.0.0.1",
      privateKey: privateKey.export({ format: "der", type: "pkcs8" }).toString("base64"),
      userHandle: randomBytes(16).toString("base64"),
      signCount: 1,
    });
  };
  /** `t5` against a stand-in that answers `behaviour`: its run, and the rows its TSV holds. */
  const t5Against = async (name: string, behaviour: OriginBehaviour) => {
    const origin = startOriginStandIn(behaviour);
    try {
      const tsv = join(scratch, `${name}.tsv`);
      const run = await drive(
        ["t5"],
        {
          DISPATCH_E2E_CASE: name,
          OUTSIDE_EDGE_STATE: stateWith({
            origin: origin.url,
            cookie: "dsession=stand-in",
            "passkey.json": passkeyRecord(),
          }),
          OUTSIDE_T5_DEADLINE: String(Math.floor(Date.now() / 1000) + 60),
          OUTSIDE_T5_TSV: tsv,
        },
        "outsideEdgeBrowser"
      );
      return { run, rows: rowsOf({ ...run, stdout: readFileSync(tsv, "utf8") }) };
    } finally {
      origin.stop();
    }
  };

  browserRow(
    "t5's passkey row is never PASS without the sign-in's own refusal of the passkey",
    async () => {
      const cases: readonly (readonly [string, OriginBehaviour, string])[] = [
        // ReviewET1b's control: the alert of a request that reached nobody, and no request.
        ["t5-unreached", {}, ""],
        ["t5-404", { finish: { status: 404 } }, "answered 404 with no code"],
        ["t5-429", { finish: { status: 429 } }, "answered 429 with no code"],
        [
          "t5-membership-unreadable",
          { finish: { status: 503, code: "MEMBERSHIP_UNREADABLE" } },
          "answered 503 MEMBERSHIP_UNREADABLE",
        ],
        [
          "t5-sign-count",
          { finish: { status: 401, code: "PASSKEY_SIGN_COUNT" } },
          "answered 401 PASSKEY_SIGN_COUNT",
        ],
        // NOT_LISTED is a sign-in's 403, so a 401 that names it is no refusal Dispatch writes.
        [
          "t5-not-listed-401",
          { finish: { status: 401, code: "NOT_LISTED" } },
          "answered 401 NOT_LISTED",
        ],
      ];
      const ran = await Promise.all(
        cases.map(async ([name, behaviour, answered]) => ({
          name,
          answered,
          ...(await t5Against(name, behaviour)),
        }))
      );
      for (const { name, answered, run, rows } of ran) {
        const passkey = rows.get("T5-passkey");
        expect(rows.get("T5-cookie")?.verdict, `${name}: ${run.stdout}`).toBe("PASS");
        expect(passkey?.verdict, `${name}: ${run.stdout}`).toBe("UNKNOWN");
        expect(passkey?.got, name).toMatch(
          answered === ""
            ? new RegExp(
                `^no answer to POST /auth/passkey/login/finish at=[0-9]{10}, so no refusal of the passkey was read; the page said: ${unreachedAlert.replaceAll(".", "\\.")}$`
              )
            : new RegExp(
                `^POST /auth/passkey/login/finish ${answered} at=[0-9]{10}, which is no refusal of the passkey itself; the page said: `
              )
        );
        expect(run.code, name).toBe(2);
      }
    },
    rowLimit({
      atOnce: [
        "outsideEdgeBrowser",
        "outsideEdgeBrowser",
        "outsideEdgeBrowser",
        "outsideEdgeBrowser",
        "outsideEdgeBrowser",
        "outsideEdgeBrowser",
      ],
    })
  );

  browserRow(
    "t5's passkey row is PASS on 401 PASSKEY_UNKNOWN and on 403 NOT_LISTED, and FAIL on a 2xx",
    async () => {
      const [unknown, notListed, accepted] = await Promise.all([
        t5Against("t5-passkey-unknown", { finish: { status: 401, code: "PASSKEY_UNKNOWN" } }),
        t5Against("t5-not-listed", { finish: { status: 403, code: "NOT_LISTED" } }),
        t5Against("t5-accepted", { finish: { status: 200 } }),
      ]);
      expect(unknown.rows.get("T5-passkey"), unknown.run.stdout).toMatchObject({
        verdict: "PASS",
      });
      expect(unknown.rows.get("T5-passkey")?.got).toMatch(
        /^refused at=[0-9]{10}: POST \/auth\/passkey\/login\/finish answered 401 PASSKEY_UNKNOWN; the page said: the stand-in answers PASSKEY_UNKNOWN$/
      );
      expect(unknown.run.code).toBe(0);
      expect(notListed.rows.get("T5-passkey")?.got, notListed.run.stdout).toMatch(
        /^refused at=[0-9]{10}: POST \/auth\/passkey\/login\/finish answered 403 NOT_LISTED; /
      );
      expect(notListed.rows.get("T5-passkey")?.verdict).toBe("PASS");
      expect(notListed.run.code).toBe(0);
      expect(accepted.rows.get("T5-passkey")?.got, accepted.run.stdout).toMatch(
        /^POST \/auth\/passkey\/login\/finish answered 200 with no code at=[0-9]{10}, a sign-in after the cookie's 401; /
      );
      expect(accepted.rows.get("T5-passkey")?.verdict).toBe("FAIL");
      expect(accepted.run.code).toBe(1);
    },
    rowLimit({ atOnce: ["outsideEdgeBrowser", "outsideEdgeBrowser", "outsideEdgeBrowser"] })
  );

  browserRow(
    "the link's token reaches no row, output or state file, where the page did not load or names the link",
    async () => {
      const canary = `canary${randomBytes(16).toString("hex")}`;
      const echo = startOriginStandIn();
      try {
        const limits = join(scratch, "leak-limits.txt");
        writeFileSync(limits, "3010 127.0.0.1 any - 1200 60\n");
        const runWith = async (name: string, invite: string) => {
          const state = join(scratch, name);
          const ran = await drive(
            [],
            {
              DISPATCH_E2E_CASE: name,
              OUTSIDE_EDGE_ALLOW_HTTP: "1",
              OUTSIDE_INVITE_URL: invite,
              OUTSIDE_PROJECT: "OWN",
              OUTSIDE_PEER_SESSION: peer,
              OUTSIDE_BROKER_URL: "http://127.0.0.1:9",
              OUTSIDE_LISTENER_URL: "http://127.0.0.1:9",
              OUTSIDE_EDGE_LIMITS: limits,
              OUTSIDE_EDGE_STATE: state,
            },
            "outsideEdgeBrowser"
          );
          return { ran, state };
        };
        const [unloaded, named] = await Promise.all([
          // Chromium refuses to load port 9, and Playwright's error names the whole URL.
          runWith("leak-unloaded", `http://127.0.0.1:9/enroll#${canary}`),
          // The stand-in's enrollment page refuses its link in an alert naming the whole link.
          runWith("leak-named", `${echo.url}/enroll#${canary}`),
        ]);
        const unloadedRow = rowsOf(unloaded.ran).get("sign-in");
        expect(unloadedRow?.verdict, unloaded.ran.stdout).toBe("UNKNOWN");
        expect(unloadedRow?.got).toStartWith("the enrollment page did not load: ");
        expect(unloadedRow?.got).toContain("/enroll#<withheld>");
        expect(unloaded.ran.code).toBe(2);
        expect(rowsOf(named.ran).get("sign-in"), named.ran.stdout).toMatchObject({
          got: `the page refused the link: This link is not one Dispatch made: ${echo.url}/enroll#<withheld>`,
          verdict: "FAIL",
        });
        expect(named.ran.code).toBe(1);
        for (const { ran, state } of [unloaded, named]) {
          expect(ran.stdout).not.toContain(canary);
          expect(ran.stderr).not.toContain(canary);
          const files = readdirSync(state);
          expect(files).toContain("outside-edge.tsv");
          for (const file of files) {
            expect(readFileSync(join(state, file), "utf8"), file).not.toContain(canary);
          }
        }
      } finally {
        echo.stop();
      }
    },
    rowLimit({ atOnce: ["outsideEdgeBrowser", "outsideEdgeBrowser"] })
  );
});

const pgProvider = process.env.DISPATCH_E2E_PG_BIN ?? process.env.DISPATCH_E2E_PG_IMAGE;
const stackRow = pgProvider === undefined ? test.skip : test;
if (pgProvider === undefined) {
  console.log(
    "outside-edge.test.ts: the stack rows are skipped: neither DISPATCH_E2E_PG_BIN nor DISPATCH_E2E_PG_IMAGE names a Postgres for the stack"
  );
}

describe("the driver against the local stack", () => {
  let runs: StackRuns;
  beforeAll(async () => {
    if (pgProvider === undefined) return;
    if (!existsSync(join(dispatchPackage, "web/dist/index.html"))) {
      throw new Error("the stack rows need the built dashboard: run `bun run build:web` first");
    }
    const child = await runBounded({
      argv: ["bun", stackProgram],
      bound: "outsideEdgeStack",
      cwd: dispatchPackage,
      env: { ...process.env, DISPATCH_E2E_SIGNIN: "google" },
      from: import.meta.url,
    });
    const last = child.stdout.trimEnd().split("\n").at(-1) ?? "";
    if (child.code !== 0 || !last.startsWith("{")) {
      throw new Error(
        `outside-edge-stack.ts exited ${child.code}:\n${child.stdout}\n${child.stderr}`
      );
    }
    runs = JSON.parse(last) as StackRuns;
  }, rowLimit("outsideEdgeStack"));

  stackRow(
    "a person on one project passes every step, and approves the tailnet leg's code beside its own",
    () => {
      const rows = rowsOf(runs.pass);
      expect(
        [...rows.entries()].filter(([, row]) => row.verdict !== "PASS"),
        runs.pass.stdout
      ).toEqual([]);
      for (const name of ["sign-in", "projects", "device-login", "tailnet-code", "relay"]) {
        expect(rows.has(name), `${name} in\n${runs.pass.stdout}`).toBe(true);
      }
      expect(rows.get("projects")?.got).toBe("[OWN]");
      expect(runs.pass.stdout).toContain(`\nAPPROVED ${runs.tailnetCode}\n`);
      expect(rows.get("relay")?.got).toMatch(
        new RegExp(`^outside-e2e pong [0-9a-f]{16} from ${peer} on the relay socket$`)
      );
      // Step 4 judged every threshold of the file, and counted requests on both hosts.
      expect([...rows.keys()].filter((name) => name.startsWith("limit-"))).toHaveLength(10);
      expect(runs.pass.stdout).toMatch(
        new RegExp(`\\noutside-edge: peak ${runs.outsideHost} [1-9][0-9]* requests in 60 s\\n`)
      );
      expect(runs.pass.stdout).toMatch(
        new RegExp(`\\noutside-edge: peak ${runs.edgeHost} [1-9][0-9]* requests in 60 s\\n`)
      );
      expect(verdictLine(runs.pass)).toBe("PASS");
      expect(runs.pass.code).toBe(0);
      // The output carries no link, token or cookie of the run.
      expect(runs.pass.stdout).not.toMatch(
        /enroll#|dsession=|edge-login-|edge-refresh-|relay-standin-/
      );
    }
  );

  stackRow("approve approves one more code in the kept browser state", () => {
    expect(runs.approve.stdout).toBe(`APPROVED ${runs.approveCode}\n`);
    expect(runs.approve.code).toBe(0);
  });

  stackRow("a person on a second project fails step 1, naming the extra project", () => {
    const rows = rowsOf(runs.secondProject);
    expect(rows.get("sign-in")?.verdict).toBe("PASS");
    expect(rows.get("projects")).toEqual({
      want: "GET /api/v1/projects lists exactly [OWN]",
      got: "[OTHER, OWN]; listed beside it: OTHER",
      verdict: "FAIL",
    });
    expect(verdictLine(runs.secondProject)).toBe("FAIL: projects");
    expect(runs.secondProject.code).toBe(1);
  });

  stackRow("a peer that never answers fails step 3 by its timeout, naming the peer", () => {
    const rows = rowsOf(runs.silentPeer);
    expect(rows.get("device-login")?.verdict).toBe("PASS");
    expect(rows.get("relay")?.got).toBe(`no reply from ${silentPeer} within 5 s`);
    expect(rows.get("relay")?.verdict).toBe("FAIL");
    expect(verdictLine(runs.silentPeer)).toBe("FAIL: relay");
    expect(runs.silentPeer.code).toBe(1);
  });

  stackRow("a threshold of 1 fails step 4, naming the host", () => {
    const rows = rowsOf(runs.oneRequest);
    expect(rows.get("relay")?.verdict).toBe("PASS");
    const row = rows.get("limit-2000-60s");
    expect(row?.verdict).toBe("FAIL");
    expect(row?.got).toMatch(new RegExp(`^peak [2-9][0-9]* in 60 s from ${runs.edgeHost}$`));
    expect(verdictLine(runs.oneRequest)).toBe("FAIL: limit-2000-60s");
    expect(runs.oneRequest.code).toBe(1);
  });

  stackRow("revoke ends every recorded login on the person's own list but the one it keeps", () => {
    const rows = rowsOf(runs.revoke);
    // The driver's own login and the tailnet leg's; approve's is OUTSIDE_EDGE_REVOKE_KEEP's.
    const revoked = [...rows.entries()].filter(([name]) => name.startsWith("revoke-"));
    expect(revoked, runs.revoke.stdout).toHaveLength(2);
    for (const [, row] of revoked) {
      expect(row).toMatchObject({
        got: "listed; the revoke answered 200, and the list read again no longer holds it",
        verdict: "PASS",
      });
    }
    expect(rows.has(`revoke-${runs.keptCredential}`)).toBe(false);
    expect(runs.revoke.code).toBe(0);
    expect(runs.keptCredential).not.toBe("");
    expect(runs.listedAfterRevoke).toEqual([runs.keptCredential]);
  });

  stackRow("t5 while the person is still listed reads UNKNOWN at its deadline, never PASS", () => {
    const rows = rowsOf({ code: 0, stdout: runs.t5Listed.tsv, stderr: "" });
    expect(rows.get("T5-cookie")).toMatchObject({
      got: "still 200 at the deadline",
      verdict: "UNKNOWN",
    });
    expect(rows.get("T5-passkey")?.verdict).toBe("UNKNOWN");
    expect(runs.t5Listed.code).toBe(2);
  });

  stackRow(
    "t5 after the owner took the person off reads the cookie's 401 and the refused passkey",
    () => {
      const rows = rowsOf({ code: 0, stdout: runs.t5Delisted.tsv, stderr: "" });
      expect(rows.get("T5-cookie")?.got, runs.t5Delisted.stdout).toMatch(/^401 at=[0-9]{10}$/);
      expect(rows.get("T5-cookie")?.verdict).toBe("PASS");
      expect(rows.get("T5-passkey")?.got, runs.t5Delisted.stdout).toMatch(
        /^refused at=[0-9]{10}: POST \/auth\/passkey\/login\/finish answered (401 PASSKEY_UNKNOWN|403 NOT_LISTED); the page said: /
      );
      expect(rows.get("T5-passkey")?.verdict).toBe("PASS");
      expect(runs.t5Delisted.code).toBe(0);
    }
  );

  stackRow(
    "t5 for a person still listed whose cookie is refused reads FAIL: the restored passkey signs them in",
    () => {
      // The positive control of the refusal above: the same restore of a kept passkey, in the
      // same fresh browser, signs in a person the owner did not take off.
      const rows = rowsOf({ code: 0, stdout: runs.t5StillListed.tsv, stderr: "" });
      expect(rows.get("T5-cookie")?.got, runs.t5StillListed.stdout).toMatch(/^401 at=[0-9]{10}$/);
      expect(rows.get("T5-passkey")?.got, runs.t5StillListed.stdout).toMatch(
        /^the passkey signed the person in again at=[0-9]{10}, after the cookie's 401$/
      );
      expect(rows.get("T5-passkey")?.verdict).toBe("FAIL");
      expect(runs.t5StillListed.code).toBe(1);
    }
  );

  test("the stack's people are four, one per case", () => {
    expect(new Set(Object.values(people)).size).toBe(4);
  });
});
