/** The outside driver, `e2e/acceptance/outside-edge.ts`: what works for a person from outside,
 *  through the edge that serves them. Its step 4's rule is `outside-edge-limits.test.ts`'s.
 *
 *  The rows that start nothing hold that a run with an input missing says UNKNOWN, naming the
 *  input, and never PASS, that OUTSIDE_EDGE_LOGIN_SECONDS is refused outside the driver's own test,
 *  and that an error of a command's own (`run`, `revoke`, `release` given a state they cannot use)
 *  is UNKNOWN, exit 2, never 1. The control rows run the driver against an outside origin
 *  stand-in
 *  (`outside-edge-origin-stand-in.ts`) that answers what a real refusal, sign-in, device login or
 *  revocation would not:
 *  - `t5`'s passkey row is PASS only on the sign-in's own refusal of the passkey, 401
 *    PASSKEY_UNKNOWN or 403 NOT_LISTED from `POST /auth/passkey/login/finish`; an alert with no
 *    answer ("Dispatch could not be reached"), a 404, a 429, any other code (a 403 among them) is
 *    UNKNOWN, and a 2xx is FAIL, also one that holds its body back; any other answer that sends
 *    its status and holds its body back is UNKNOWN, written before the deadline;
 *  - `revoke` passes a credential only when the list, read again, no longer holds it, and a list
 *    that gives no answer, before the revoke or after it, is UNKNOWN;
 *  - step 1 after its page loaded is FAIL or UNKNOWN, never PASS, for each way a sign-in falls
 *    short: no Create button, no sign-in, a refusal, no passkey, a landing on another origin, no
 *    cookie, no person, a page or a read with no answer; and a read whose status came and whose
 *    body never does is FAIL, and a project list that is no answer fails `projects`; a browser
 *    that ends while a page step waits is UNKNOWN, the driver's own failure; and a page that
 *    holds its main thread while the driver reads through it or saves the browser's state is
 *    UNKNOWN at the driver's own bound, naming what it held;
 *  - step 2 is FAIL or UNKNOWN for each way a device login falls short: no broker, a broker that
 *    takes the connection and never answers, a broker name that does not resolve, a broker that
 *    answers no poll by the deadline, or answers and then falls silent (each UNKNOWN), a refusal,
 *    a page on another origin, no code box, a box that takes no typing, no answer to the look-up,
 *    a refused code or approval, no approval shown, no credential named, a denied poll, a poll
 *    answered pending to the deadline; a browser that ends while the code box or the credential
 *    is awaited is UNKNOWN; and `approve` reads a page it cannot drive UNKNOWN, exit 2, and prints
 *    a refusal whose text holds a line break on one line;
 *  - step 3, after a device login that passed, is UNKNOWN, never FAIL, for each far end that gave
 *    no answer: a listener that refused the socket's connection, a trade for the session's token
 *    the broker never answered, and a listener that never answered an upgrade still waiting at the
 *    driver's 30 s; and a listener's refusal of the registration is FAIL even where its body
 *    names a code of a request that got no answer, and so is a registration answered with a
 *    status whose body never comes, 503 or 200;
 *  - an error of the driver's own in a step, a device login or a session the listener took that
 *    it cannot keep in OUTSIDE_EDGE_STATE, is UNKNOWN, exit 2, never FAIL, and leaves no copy of
 *    the login in the state; an approved code it cannot record is named on stderr first, and
 *    `run` and `approve` read it UNKNOWN, exit 2, naming the state's file;
 *  - Chromium's processes hold none of the driver's OUTSIDE_* inputs;
 *  - the link's token reaches no row, stdout, stderr, TSV or state file, also where the page did
 *    not load or names the link itself;
 *  - the request counter sees the relay socket the driver's relay client opens.
 *  The rows that wait out one of the driver's own bounds of real time, or a fixed wait inside
 *  one, skip under DISPATCH_UNIT_REPEAT (`realTimeRow`, `realTimeBrowserRow`), as the hold's do.
 *  A row reads the fake broker's revoke of a machine login (`e2e/fake-broker.ts`), which the
 *  stack's Dispatch relays the driver's revoke to: 403 NOT_APPROVER for another person's, 404 for
 *  no login, 200 for the person's own, as the broker answers.
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
import { hostname, tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { legacyAgentSubject } from "@legion/contracts";
import { startRelaySocketStandIn } from "@legion/envoy-client/test/relay-socket-stand-in";
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import { chromium } from "@playwright/test";
import { readInPage } from "../acceptance/outside-edge-page-read";
import {
  type BoundedRun,
  type BoundName,
  beforeDeadline,
  eventually,
  pastDeadline,
  rowLimit,
  runBounded,
} from "./bounded-subprocess";
import { sessionRefusal, topicRefusal, tradeRefusal } from "./outside-edge-credentials";
import {
  type OriginBehaviour,
  type OriginStandIn,
  standInCredential,
  standInPerson,
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
): Promise<BoundedRun> {
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

/** A run's TSV rows by name. */
type RunRows = Map<string, { want: string; got: string; verdict: string }>;

/** The TSV rows a run printed, by name: `row`, `want`, `got`, verdict. */
function rowsOf(run: DriverRun): RunRows {
  const rows: RunRows = new Map();
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

/** A process of this machine: its pid and its command line. */
interface ProcessLine {
  readonly pid: number;
  readonly command: string;
}

/** The command line of process `pid`, its arguments joined by spaces; "" where it is gone. */
function commandOf(pid: number): string {
  try {
    return readFileSync(`/proc/${pid}/cmdline`, "utf8").replaceAll("\0", " ").trim();
  } catch {
    return "";
  }
}

/** The processes whose environment holds `variable` (`NAME=value`) exactly, each that this user
 *  may read: every other is passed over. */
function holdersOf(variable: string): ProcessLine[] {
  const holders: ProcessLine[] = [];
  for (const entry of readdirSync("/proc")) {
    if (!/^[0-9]+$/.test(entry)) continue;
    let environ: string;
    try {
      environ = readFileSync(`/proc/${entry}/environ`, "utf8");
    } catch {
      continue;
    }
    if (environ.split("\0").includes(variable)) {
      holders.push({ pid: Number(entry), command: commandOf(Number(entry)) });
    }
  }
  return holders;
}

/** Every process `root` started, and those they started, read from each process's parent
 *  (field 4 of `/proc/<pid>/stat`). */
function descendantsOf(root: number): ProcessLine[] {
  const parents = new Map<number, number>();
  for (const entry of readdirSync("/proc")) {
    if (!/^[0-9]+$/.test(entry)) continue;
    try {
      const stat = readFileSync(`/proc/${entry}/stat`, "utf8");
      parents.set(Number(entry), Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[1]));
    } catch {
      // Gone since the listing.
    }
  }
  const descendants: ProcessLine[] = [];
  for (const pid of parents.keys()) {
    for (let at = parents.get(pid); at !== undefined && at > 1; at = parents.get(at)) {
      if (at === root) {
        descendants.push({ pid, command: commandOf(pid) });
        break;
      }
    }
  }
  return descendants;
}

/** The Chromium processes of the driver run whose OUTSIDE_EDGE_STATE is `state`. */
function browserOf(state: string): ProcessLine[] {
  const driverProcess = holdersOf(`OUTSIDE_EDGE_STATE=${state}`).find((one) =>
    one.command.includes("outside-edge.ts")
  );
  if (driverProcess === undefined) throw new Error("no driver process holds the row's state");
  return descendantsOf(driverProcess.pid).filter((one) =>
    /chrom|headless_shell/i.test(one.command)
  );
}

/** Ends, by SIGKILL, every Chromium process of the driver run whose OUTSIDE_EDGE_STATE is
 *  `state`, as a browser that crashed ends; answers how many it found running. */
function endBrowserOf(state: string): number {
  const browser = browserOf(state);
  for (const one of browser) {
    try {
      process.kill(one.pid, "SIGKILL");
    } catch {
      // It ended since the read, with the browser's other processes.
    }
  }
  return browser.length;
}

/** Whether process `pid` runs: listed in /proc, and no zombie. */
function running(pid: number): boolean {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf8");
    return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[0] !== "Z";
  } catch {
    return false;
  }
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

  test(
    "OUTSIDE_EDGE_LOGIN_SECONDS, the driver's own test's, is refused without OUTSIDE_EDGE_ALLOW_HTTP, and outside 1 to 300",
    async () => {
      const [unasked, none, over] = await Promise.all([
        drive([], { ...all, OUTSIDE_EDGE_LOGIN_SECONDS: "5" }, "short"),
        drive(
          [],
          { ...all, OUTSIDE_EDGE_ALLOW_HTTP: "1", OUTSIDE_EDGE_LOGIN_SECONDS: "0" },
          "short"
        ),
        drive(
          [],
          { ...all, OUTSIDE_EDGE_ALLOW_HTTP: "1", OUTSIDE_EDGE_LOGIN_SECONDS: "301" },
          "short"
        ),
      ]);
      const refused = (said: string) =>
        `inputs\tevery input of the driver\t${said}\tUNKNOWN\nUNKNOWN: inputs\n`;
      expect(unasked.stdout).toBe(
        refused(
          "OUTSIDE_EDGE_LOGIN_SECONDS is the driver's own test's, taken only with OUTSIDE_EDGE_ALLOW_HTTP=1"
        )
      );
      expect(unasked.code).toBe(2);
      for (const ran of [none, over]) {
        expect(ran.stdout).toBe(
          refused("OUTSIDE_EDGE_LOGIN_SECONDS must be a whole number of seconds from 1 to 300")
        );
        expect(ran.code).toBe(2);
      }
    },
    rowLimit({ atOnce: ["short", "short", "short"] })
  );

  // An error of a command's own judged nothing of the edge, so each command ends it UNKNOWN, exit
  // 2, never 1, which is a FAIL. Each case makes it as root too, which a directory's mode does
  // not stop: a file of OUTSIDE_EDGE_STATE that is a directory, or a state that is a file. Each
  // fails before the command asks anything, so none needs a stand-in. The state or its file is
  // named where the runtime's error names its path; Bun 1.4.4's read of a directory names none.
  test(
    "an error of a command's own is UNKNOWN, exit 2, naming the state or its file: run, revoke and release",
    async () => {
      const own = (name: string) => {
        const state = join(scratch, name);
        mkdirSync(state, { mode: 0o700 });
        return state;
      };
      const runState = own("own-run");
      mkdirSync(join(runState, "outside-edge.tsv"));
      const stateFile = join(scratch, "own-state-file");
      writeFileSync(stateFile, "no directory", { mode: 0o600 });
      const revokeState = own("own-revoke");
      for (const [file, text] of [
        ["origin", "https://dispatch.edge.example"],
        ["cookie", "dsession=a-cookie"],
        ["person", standInPerson],
        ["approved.tsv", "ABCD-EFGH\tcred-a\n"],
      ] as const) {
        writeFileSync(join(revokeState, file), text, { mode: 0o600 });
      }
      mkdirSync(join(revokeState, "outside-edge-revocations.tsv"));
      const releaseState = own("own-release");
      mkdirSync(join(releaseState, "holder"));
      const [run, runOnFile, revoke, release] = await Promise.all([
        drive([], { ...all, OUTSIDE_EDGE_STATE: runState }, "short"),
        drive([], { ...all, OUTSIDE_EDGE_STATE: stateFile }, "short"),
        drive(["revoke"], { OUTSIDE_EDGE_STATE: revokeState }, "short"),
        drive(["release"], { OUTSIDE_EDGE_STATE: releaseState }, "short"),
      ]);
      const wants: readonly (readonly [DriverRun, RegExp])[] = [
        [
          run,
          /^UNKNOWN: run failed on an error of its own: open EISDIR on OUTSIDE_EDGE_STATE's outside-edge\.tsv\n$/,
        ],
        [
          runOnFile,
          /^UNKNOWN: run failed on an error of its own: mkdir EEXIST on OUTSIDE_EDGE_STATE\n$/,
        ],
        [
          revoke,
          /^UNKNOWN: revoke failed on an error of its own: open EISDIR on OUTSIDE_EDGE_STATE's outside-edge-revocations\.tsv\n$/,
        ],
        [release, /^UNKNOWN: release failed on an error of its own: read EISDIR\n$/],
      ];
      for (const [ran, want] of wants) {
        expect(ran.stdout, ran.stderr).toMatch(want);
        expect(ran.code, ran.stdout).toBe(2);
      }
    },
    rowLimit({ atOnce: ["short", "short", "short", "short"] })
  );
});

describe("the read through the page", () => {
  test(
    "ends at the deadline the driver set as it asked, however late the page started it, as a read that got no answer",
    async () => {
      // A listener that takes the connection and never answers a byte. The script `pageRead`
      // hands the page runs here as the page would run it, started as late as a page whose main
      // thread was busy when the driver asked: 1 s of the driver's deadline left. Counted from
      // its own start, the read would wait its 30 s, past the driver's 35 s for the script.
      const silent = Bun.listen({
        hostname: "127.0.0.1",
        port: 0,
        socket: { data: () => undefined },
      });
      try {
        const started = Date.now();
        const read = await readInPage({
          at: `http://127.0.0.1:${silent.port}/auth/whoami`,
          until: started + 1_000,
        });
        const took = Date.now() - started;
        expect(read).toEqual({ unanswered: expect.stringMatching(/^TimeoutError: /) });
        expect(took, "the read's time").toBeLessThan(10_000);
      } finally {
        silent.stop(true);
      }
    },
    rowLimit("short")
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
/** Under DISPATCH_UNIT_REPEAT, which `bun run test:repeat` sets (`e2e/tools/unit-repeat.ts`), the
 *  rows that wait out one of the driver's own bounds of real time, or a fixed wait inside one,
 *  skip, as the hold's real-time rows do (`outside-edge-hold.test.ts`'s head): the repeat lane
 *  runs this directory twenty times over, and those waits would spend minutes of it each run,
 *  finding no fault a single run does not. Each runs once a run wherever the rest of this file
 *  runs. */
const repeated = process.env.DISPATCH_UNIT_REPEAT === "1";
const realTimeRow = repeated ? test.skip : test;
const realTimeBrowserRow = repeated || !chromiumHere ? test.skip : test;

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
    "revoke keeps the login OUTSIDE_EDGE_REVOKE_KEEP names in either form of its code, and revokes the rest",
    async () => {
      const origin = startOriginStandIn({ listed: ["cred-a", "cred-b"], revokeRemoves: true });
      try {
        const state = stateWith({
          origin: origin.url,
          cookie: "dsession=stand-in",
          person: "carol@outside.example",
          "approved.tsv": "ABCD-EFGH\tcred-a\nWXYZ-WXYZ\tcred-b\n",
        });
        // The kept code as the broker prints it without its hyphen and in lower case: one code.
        const run = await drive(
          ["revoke"],
          { OUTSIDE_EDGE_STATE: state, OUTSIDE_EDGE_REVOKE_KEEP: "abcdefgh" },
          "short"
        );
        expect([...rowsOf(run).keys()], run.stdout).toEqual(["revoke-cred-b"]);
        expect(rowsOf(run).get("revoke-cred-b")?.verdict).toBe("PASS");
        expect(run.code).toBe(0);
        expect(origin.revoked).toEqual(["cred-b"]);
      } finally {
        origin.stop();
      }
    },
    rowLimit("short")
  );

  test(
    "t5 and revoke refuse an empty kept cookie, UNKNOWN, rather than read with no cookie",
    async () => {
      const origin = startOriginStandIn({ listed: ["cred-a"], revokeRemoves: true });
      try {
        const tsv = join(scratch, "empty-cookie.tsv");
        const [t5Run, revokeRun] = await Promise.all([
          drive(
            ["t5"],
            {
              OUTSIDE_EDGE_STATE: stateWith({
                origin: origin.url,
                cookie: "",
                "passkey.json": "{}",
              }),
              OUTSIDE_T5_DEADLINE: String(Math.floor(Date.now() / 1000) + 60),
              OUTSIDE_T5_TSV: tsv,
            },
            "short"
          ),
          drive(
            ["revoke"],
            {
              OUTSIDE_EDGE_STATE: stateWith({
                origin: origin.url,
                cookie: "",
                person: "carol@outside.example",
                "approved.tsv": "ABCD-EFGH\tcred-a\n",
              }),
            },
            "short"
          ),
        ]);
        const empty = "OUTSIDE_EDGE_STATE's cookie is empty: the run kept none";
        expect(rowsOf(t5Run).get("T5-cookie"), t5Run.stdout).toMatchObject({
          got: empty,
          verdict: "UNKNOWN",
        });
        expect(t5Run.code).toBe(2);
        expect(rowsOf(revokeRun).get("revoke"), revokeRun.stdout).toMatchObject({
          got: empty,
          verdict: "UNKNOWN",
        });
        expect(revokeRun.code).toBe(2);
        // Neither read anything with the empty cookie.
        expect(origin.asked).toEqual([]);
      } finally {
        origin.stop();
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

  realTimeRow(
    "revoke reads UNKNOWN, not PASS, when the list read again after the revoke gives no answer",
    async () => {
      // The control is the first row: a list that answers, read again, passes the revoke.
      const silent = startOriginStandIn({
        listed: ["cred-a"],
        revokeRemoves: true,
        silentAfterRevoke: true,
      });
      try {
        const run = await drive(
          ["revoke"],
          { OUTSIDE_EDGE_STATE: revokeState(silent.url) },
          "outsideEdgePageWait"
        );
        const row = rowsOf(run).get("revoke-cred-a");
        expect(row?.got, run.stdout).toStartWith(
          "listed; the revoke answered 200, then the list read again: GET /api/v1/machine-logins got no answer: "
        );
        expect(row?.verdict).toBe("UNKNOWN");
        expect(verdictLine(run)).toBe("UNKNOWN: revoke-cred-a");
        expect(run.code).toBe(2);
        expect(silent.revoked).toEqual(["cred-a"]);
      } finally {
        silent.stop();
      }
    },
    rowLimit("outsideEdgePageWait")
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
        // NOT_LISTED is a sign-in's 403, so a 401 that names it is no refusal Dispatch writes,
        // and a 403 that names another code is none either.
        [
          "t5-not-listed-401",
          { finish: { status: 401, code: "NOT_LISTED" } },
          "answered 401 NOT_LISTED",
        ],
        [
          "t5-another-owner",
          { finish: { status: 403, code: "LISTED_BY_ANOTHER_OWNER" } },
          "answered 403 LISTED_BY_ANOTHER_OWNER",
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

  realTimeBrowserRow(
    "t5 reads UNKNOWN before its deadline where the sign-in's last answer sends its status and holds its body back",
    async () => {
      // Once its body came, the answer would be a refusal of the passkey (401 PASSKEY_UNKNOWN), but
      // the body never comes, nor the stand-in page's alert, which waits on it. The deadline is 25
      // s off, so every wait of the sign-in ends 2 s before it: the run must end by then with the
      // row written, where a body read with no bound of its own holds it until its child's bound.
      const origin = startOriginStandIn({
        finish: { status: 401, code: "PASSKEY_UNKNOWN", stallBody: true },
      });
      try {
        const tsv = join(scratch, "t5-stalled-body.tsv");
        const deadline = Math.floor(Date.now() / 1000) + 25;
        const run = await drive(
          ["t5"],
          {
            DISPATCH_E2E_CASE: "t5-stalled-body",
            OUTSIDE_EDGE_STATE: stateWith({
              origin: origin.url,
              cookie: "dsession=stand-in",
              "passkey.json": passkeyRecord(),
            }),
            OUTSIDE_T5_DEADLINE: String(deadline),
            OUTSIDE_T5_TSV: tsv,
          },
          "outsideEdgePageWait"
        );
        // The driver's own end, before the harness killed what it left (`BoundedRun`).
        const endedAt = run.endedAt / 1000;
        const passkey = rowsOf({ ...run, stdout: readFileSync(tsv, "utf8") }).get("T5-passkey");
        const got =
          /^POST \/auth\/passkey\/login\/finish answered 401 at=([0-9]{10}), and its body did not come in the [0-9]+ s left before the deadline, so no refusal of the passkey was read$/.exec(
            passkey?.got ?? ""
          );
        expect(got, run.stdout).not.toBeNull();
        expect(passkey?.verdict).toBe("UNKNOWN");
        // Written before the deadline, and the run ended within its browser's close after it.
        expect(Number(got?.[1])).toBeLessThanOrEqual(deadline);
        expect(endedAt).toBeLessThan(deadline + 10);
        expect(run.code).toBe(2);
      } finally {
        origin.stop();
      }
    },
    rowLimit("outsideEdgePageWait")
  );

  realTimeBrowserRow(
    "t5 reads FAIL before its deadline where the sign-in's last answer is a 2xx that holds its body back",
    async () => {
      // A 2xx is a sign-in whatever its body would name: its status comes, and its body never
      // does, nor the stand-in page's alert, which waits on it. The deadline is 25 s off, so the
      // run ends by then with the row written, and the verdict is the status's.
      const origin = startOriginStandIn({
        finish: { status: 200, code: "SIGNED_IN", stallBody: true },
      });
      try {
        const tsv = join(scratch, "t5-stalled-2xx.tsv");
        const deadline = Math.floor(Date.now() / 1000) + 25;
        const run = await drive(
          ["t5"],
          {
            DISPATCH_E2E_CASE: "t5-stalled-2xx",
            OUTSIDE_EDGE_STATE: stateWith({
              origin: origin.url,
              cookie: "dsession=stand-in",
              "passkey.json": passkeyRecord(),
            }),
            OUTSIDE_T5_DEADLINE: String(deadline),
            OUTSIDE_T5_TSV: tsv,
          },
          "outsideEdgePageWait"
        );
        // The driver's own end, before the harness killed what it left (`BoundedRun`).
        const endedAt = run.endedAt / 1000;
        const passkey = rowsOf({ ...run, stdout: readFileSync(tsv, "utf8") }).get("T5-passkey");
        const got =
          /^POST \/auth\/passkey\/login\/finish answered 200 at=([0-9]{10}), a sign-in after the cookie's 401, though its body did not come in the [0-9]+ s left before the deadline$/.exec(
            passkey?.got ?? ""
          );
        expect(got, run.stdout).not.toBeNull();
        expect(passkey?.verdict).toBe("FAIL");
        expect(Number(got?.[1])).toBeLessThanOrEqual(deadline);
        expect(endedAt).toBeLessThan(deadline + 10);
        expect(run.code).toBe(1);
      } finally {
        origin.stop();
      }
    },
    rowLimit("outsideEdgePageWait")
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

  /** A run of the driver against an origin stand-in that answers `behaviour`: its link that
   *  stand-in's, its broker that stand-in unless `broker` names another, its listener a port
   *  nothing answers on unless `listener` names another, and `env` beside its inputs. Answers the
   *  stand-in at once, the run's state directory, and the run. */
  const startRun = (
    name: string,
    behaviour: OriginBehaviour,
    bound: BoundName,
    broker?: string,
    listener?: string,
    env: Record<string, string> = {}
  ): { origin: OriginStandIn; state: string; ran: Promise<DriverRun> } => {
    const origin = startOriginStandIn(behaviour);
    const state = join(scratch, name);
    const limits = join(scratch, `${name}-limits.txt`);
    writeFileSync(limits, "3010 127.0.0.1 any - 1200 60\n");
    const ran = drive(
      [],
      {
        DISPATCH_E2E_CASE: name,
        OUTSIDE_EDGE_ALLOW_HTTP: "1",
        OUTSIDE_INVITE_URL: `${origin.page}/enroll#token-of-${name}`,
        OUTSIDE_PROJECT: "OWN",
        OUTSIDE_PEER_SESSION: peer,
        OUTSIDE_BROKER_URL: broker ?? origin.url,
        OUTSIDE_LISTENER_URL: listener ?? "http://127.0.0.1:9",
        OUTSIDE_EDGE_LIMITS: limits,
        OUTSIDE_EDGE_STATE: state,
        ...env,
      },
      bound
    );
    // Read by the row once it awaits it; stopped there too, however the run ended.
    ran.catch(() => undefined);
    return { origin, state, ran };
  };
  /** Each case's run, its rows and its stand-in, all started at once and awaited together, each
   *  with `env` beside its inputs. */
  const runCases = async (
    cases: readonly (readonly [string, OriginBehaviour, string?, string?])[],
    bound: BoundName,
    env: Record<string, string> = {}
  ) => {
    const started = cases.map(([name, behaviour, broker, listener]) => ({
      name,
      ...startRun(name, behaviour, bound, broker, listener, env),
    }));
    try {
      return await Promise.all(
        started.map(async ({ name, origin, ran }) => {
          const run = await ran;
          return { name, origin, run, rows: rowsOf(run) };
        })
      );
    } finally {
      for (const { origin } of started) origin.stop();
    }
  };
  /** The verdict and the text of row `row` of a run, for a failure message that names the case. */
  const rowOf = (ran: { name: string; run: DriverRun; rows: RunRows }, row: string) => {
    const found = ran.rows.get(row);
    if (found === undefined) throw new Error(`${ran.name}: no row ${row} in\n${ran.run.stdout}`);
    return found;
  };

  browserRow(
    "the sign-in step after its page loaded: each refusal, wrong origin, missing passkey, cookie or person is FAIL, never PASS",
    async () => {
      const ran = await runCases(
        [
          ["sign-in-refused", { enroll: "refused" }],
          ["sign-in-no-passkey", { enroll: "no-passkey" }],
          ["sign-in-elsewhere", { enroll: "elsewhere" }],
          ["sign-in-whoami-403", { enroll: "signs-in", whoami: 403 }],
          ["sign-in-no-cookie", { enroll: "no-cookie" }],
        ],
        "outsideEdgeBrowser"
      );
      const wants: Record<string, RegExp> = {
        "sign-in-refused": /^the enrollment was refused: This invitation was withdrawn\.$/,
        "sign-in-no-passkey": /^the authenticator holds 0 credentials after the enrollment, not 1$/,
        "sign-in-elsewhere":
          /^the passkey signed the person in at http:\/\/127\.0\.0\.1:[0-9]+, not at the link's origin$/,
        "sign-in-whoami-403": /^GET \/auth\/whoami answered 403 with no login$/,
        "sign-in-no-cookie": /^the sign-in set no cookie for http:\/\/localhost:[0-9]+$/,
      };
      for (const one of ran) {
        const signIn = rowOf(one, "sign-in");
        expect(signIn.verdict, `${one.name}: ${one.run.stdout}`).toBe("FAIL");
        expect(signIn.got, one.name).toMatch(wants[one.name] ?? /^$/);
        expect(rowOf(one, "device-login"), one.name).toMatchObject({
          got: "not run: the sign-in did not pass",
          verdict: "UNKNOWN",
        });
        expect(verdictLine(one.run), one.name).toStartWith("FAIL: sign-in");
        expect(one.run.code, one.name).toBe(1);
      }
      // The landing origin the row names is the stand-in's other one, not the link's.
      const elsewhere = ran.find((one) => one.name === "sign-in-elsewhere");
      if (elsewhere === undefined) throw new Error("no elsewhere case");
      expect(rowOf(elsewhere, "sign-in").got).toContain(elsewhere.origin.elsewhere);
    },
    rowLimit({
      atOnce: [
        "outsideEdgeBrowser",
        "outsideEdgeBrowser",
        "outsideEdgeBrowser",
        "outsideEdgeBrowser",
        "outsideEdgeBrowser",
      ],
    })
  );

  realTimeBrowserRow(
    "the sign-in step that waits out its bound: no Create button or no sign-in is FAIL, a page or read with no end is UNKNOWN, a read whose status came and whose body never does is FAIL; Chromium holds none of the driver's inputs",
    async () => {
      const cases = [
        ["sign-in-no-button", { enroll: "no-button" }],
        ["sign-in-never-loads", { enroll: "never-loads" }],
        ["sign-in-no-sign-in", { enroll: "no-sign-in" }],
        ["sign-in-whoami-silent", { enroll: "signs-in", whoami: "silent" }],
        // The status came, so the far end answered, and its body never does.
        ["sign-in-whoami-stalled", { enroll: "signs-in", whoami: "stalled" }],
      ] as const;
      const started = cases.map(([name, behaviour]) => ({
        name,
        ...startRun(name, behaviour, "outsideEdgePageWait"),
      }));
      try {
        // While the first run's browser waits on a page with no button: no process the driver
        // started holds OUTSIDE_EDGE_STATE, which the driver's own process does (the control
        // that the read sees a variable where it is).
        const waiting = started[0];
        if (waiting === undefined) throw new Error("no case started");
        await eventually("the page with no button, served", () =>
          waiting.origin.asked.includes("/enroll")
        );
        const held = holdersOf(`OUTSIDE_EDGE_STATE=${waiting.state}`);
        const driverProcess = held.find((one) => one.command.includes("outside-edge.ts"));
        expect(driverProcess, JSON.stringify(held)).toBeDefined();
        const browser = descendantsOf(driverProcess?.pid ?? 0).filter((one) =>
          /chrom|headless_shell/i.test(one.command)
        );
        expect(browser.length, "Chromium of the run, running").toBeGreaterThan(0);
        expect(held.filter((one) => one.pid !== driverProcess?.pid)).toEqual([]);
        const ran = await Promise.all(
          started.map(async ({ name, ran }) => {
            const run = await ran;
            return { name, run, rows: rowsOf(run) };
          })
        );
        const wants: Record<string, readonly [RegExp, string]> = {
          "sign-in-no-button": [
            /^no enrollment: the page shows no Create a passkey button$/,
            "FAIL",
          ],
          "sign-in-never-loads": [
            /^the enrollment page did not load: .*\/enroll#<withheld>/,
            "UNKNOWN",
          ],
          "sign-in-no-sign-in": [/^the passkey enrolled no sign-in within 30 s$/, "FAIL"],
          "sign-in-whoami-silent": [/^GET \/auth\/whoami got no answer: TimeoutError/, "UNKNOWN"],
          "sign-in-whoami-stalled": [
            /^GET \/auth\/whoami answered 200, and its body did not come \(TimeoutError: [^)]*\), so it named no login$/,
            "FAIL",
          ],
        };
        for (const one of ran) {
          const [got, verdict] = wants[one.name] ?? [/^$/, ""];
          const signIn = rowOf(one, "sign-in");
          expect(signIn.verdict, `${one.name}: ${one.run.stdout}`).toBe(verdict);
          expect(signIn.got, one.name).toMatch(got);
          expect(verdictLine(one.run), one.name).not.toBe("PASS");
          expect(one.run.code, one.name).toBe(verdict === "FAIL" ? 1 : 2);
        }
      } finally {
        for (const { origin } of started) origin.stop();
      }
    },
    rowLimit("short", {
      atOnce: [
        "outsideEdgePageWait",
        "outsideEdgePageWait",
        "outsideEdgePageWait",
        "outsideEdgePageWait",
        "outsideEdgePageWait",
      ],
    })
  );

  browserRow(
    "the device login step: a refusal, another origin's page, a refused code or approval and a denied poll are FAIL; no broker, a broker name that does not resolve, or a page that cannot be driven, is UNKNOWN",
    async () => {
      const signsIn = { enroll: "signs-in" } as const;
      const ran = await runCases(
        [
          ["login-refused", { ...signsIn, broker: "refuses" }],
          ["login-elsewhere", { ...signsIn, broker: "elsewhere" }],
          ["login-refuses-code", { ...signsIn, machine: "refuses-code" }],
          ["login-refuses-approval", { ...signsIn, machine: "refuses-approval" }],
          ["login-denied", { ...signsIn, broker: "denies" }],
          ["login-projects-500", { ...signsIn, projects: 500 }, "http://127.0.0.1:9"],
          // A name under `.invalid`, which no resolver answers (RFC 6761).
          ["login-broker-unresolved", signsIn, "https://edge.invalid"],
          ["login-untypable", { ...signsIn, machine: "untypable" }],
        ],
        "outsideEdgeBrowser"
      );
      const wants: Record<string, readonly [RegExp, string]> = {
        "login-refused": [/access_denied|refuses this device login/, "FAIL"],
        "login-elsewhere": [/^the broker named its page on http:\/\/127\.0\.0\.1:[0-9]+$/, "FAIL"],
        "login-refuses-code": [
          /^the page refused the code: No machine login waits on that code\.$/,
          "FAIL",
        ],
        "login-refuses-approval": [
          /^the approval was refused: The approval was refused\.$/,
          "FAIL",
        ],
        "login-denied": [
          /^approved on the page, then the poll: this machine's device login was denied/,
          "FAIL",
        ],
        // A broker address nothing answers on: the cipher-client keeps the failure's code, which
        // the driver reads, and names it by its class in the text.
        "login-projects-500": [
          /^ask for a code for this machine's device login at .*: (ECONNREFUSED|ConnectionRefused)$/,
          "UNKNOWN",
        ],
        // EAI_AGAIN where the resolver gives no answer at all, as on a box with no DNS.
        "login-broker-unresolved": [
          /^ask for a code for this machine's device login at https:\/\/edge\.invalid\/v1\/oauth\/device_authorization: getaddrinfo (ENOTFOUND|EAI_AGAIN)$/,
          "UNKNOWN",
        ],
        "login-untypable": [/^the machine-login page could not be driven: /, "UNKNOWN"],
      };
      for (const one of ran) {
        const [got, verdict] = wants[one.name] ?? [/^$/, ""];
        const login = rowOf(one, "device-login");
        expect(login.verdict, `${one.name}: ${one.run.stdout}`).toBe(verdict);
        expect(login.got, one.name).toMatch(got);
        expect(rowOf(one, "relay"), one.name).toMatchObject({
          got: "not run: the device login did not pass",
          verdict: "UNKNOWN",
        });
        expect(verdictLine(one.run), one.name).not.toBe("PASS");
      }
      // The step before it: a project list that is no answer fails `projects` beside a sign-in
      // that passed, and a broker nothing answers on leaves the device login UNKNOWN.
      const listless = ran.find((one) => one.name === "login-projects-500");
      if (listless === undefined) throw new Error("no projects case");
      expect(rowOf(listless, "sign-in").verdict).toBe("PASS");
      expect(rowOf(listless, "projects")).toMatchObject({
        got: "answered 500, no list",
        verdict: "FAIL",
      });
      expect(rowOf(listless, "sign-in").got).toBe(
        `signed in at ${listless.origin.page} as the invited person`
      );
      // Only the poll the broker denied had its code approved on the page and recorded; a page
      // that refused recorded none.
      for (const one of ran) {
        expect(existsSync(join(scratch, one.name, "approved.tsv")), one.name).toBe(
          one.name === "login-denied"
        );
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
        "outsideEdgeBrowser",
        "outsideEdgeBrowser",
      ],
    })
  );

  realTimeBrowserRow(
    "the device login step that waits out its bound: no code box, no answer to the look-up, no approval shown, no credential named, each FAIL; a broker that takes the connection and never answers is UNKNOWN",
    async () => {
      const signsIn = { enroll: "signs-in" } as const;
      // A broker that takes the connection and never answers a byte: the device authorization
      // waits out the broker client's own 15 s, which openid-client names OAUTH_TIMEOUT and the
      // broker client keeps as the code ETIMEDOUT.
      const silent = Bun.listen({
        hostname: "127.0.0.1",
        port: 0,
        socket: { data: () => undefined },
      });
      try {
        const ran = await runCases(
          [
            ["login-no-box", { ...signsIn, machine: "no-box" }],
            ["login-no-answer", { ...signsIn, machine: "no-answer" }],
            ["login-never-approved", { ...signsIn, machine: "never-approved" }],
            // The page reads the credential's line for its 30 s before it says there is none.
            ["login-no-credential", { ...signsIn, machine: "no-credential" }],
            ["login-broker-silent", signsIn, `http://127.0.0.1:${silent.port}`],
          ],
          "outsideEdgePageWait"
        );
        const wants: Record<string, readonly [string, string]> = {
          "login-no-box": ["the machine-login page shows no code box", "FAIL"],
          "login-no-answer": ["the page showed neither Approve nor a refusal for the code", "FAIL"],
          "login-never-approved": ["the page did not say the code was approved", "FAIL"],
          "login-no-credential": ["the page approved the code and named no credential", "FAIL"],
          "login-broker-silent": [
            `ask for a code for this machine's device login at http://127.0.0.1:${silent.port}/v1/oauth/device_authorization: OAUTH_TIMEOUT`,
            "UNKNOWN",
          ],
        };
        for (const one of ran) {
          const [got, verdict] = wants[one.name] ?? ["", ""];
          expect(rowOf(one, "device-login"), `${one.name}: ${one.run.stdout}`).toMatchObject({
            got,
            verdict,
          });
          expect(verdictLine(one.run), one.name).toStartWith(`${verdict}: device-login`);
          expect(one.run.code, one.name).toBe(verdict === "FAIL" ? 1 : 2);
        }
      } finally {
        silent.stop(true);
      }
    },
    rowLimit({
      atOnce: [
        "outsideEdgePageWait",
        "outsideEdgePageWait",
        "outsideEdgePageWait",
        "outsideEdgePageWait",
        "outsideEdgePageWait",
      ],
    })
  );

  realTimeBrowserRow(
    "the device login step at its deadline: a broker that answered no poll, or answered and then fell silent, is UNKNOWN; one that answered every poll with no approval is FAIL",
    async () => {
      // OUTSIDE_EDGE_LOGIN_SECONDS, which only the driver's own test may set, makes the poll's
      // 5-minute deadline 10 s. A poll the broker never answers waits out the broker client's own
      // bound for the request, so the two silent cases end that long after the deadline, and the
      // pending one at the first poll after it.
      const signsIn = { enroll: "signs-in" } as const;
      const ran = await runCases(
        [
          ["login-poll-silent", { ...signsIn, poll: "silent" }],
          ["login-poll-once", { ...signsIn, poll: "once" }],
          ["login-poll-pending", { ...signsIn, poll: "pending" }],
        ],
        "outsideEdgePageWait",
        { OUTSIDE_EDGE_LOGIN_SECONDS: "10" }
      );
      const wants: Record<string, readonly [RegExp, string]> = {
        "login-poll-silent": [
          /^approved on the page, then the broker answered no poll within 10 s: /,
          "UNKNOWN",
        ],
        "login-poll-once": [
          /^approved on the page, then the broker answered 1 of the [0-9]+ polls, but not the last before the 10 s deadline: /,
          "UNKNOWN",
        ],
        "login-poll-pending": [
          /^approved on the page, then the poll: the poll got no approval within 10 s$/,
          "FAIL",
        ],
      };
      for (const one of ran) {
        const [got, verdict] = wants[one.name] ?? [/^$/, ""];
        const login = rowOf(one, "device-login");
        expect(login.verdict, `${one.name}: ${one.run.stdout}`).toBe(verdict);
        expect(login.got, one.name).toMatch(got);
        expect(verdictLine(one.run), one.name).toStartWith(`${verdict}: device-login`);
        expect(one.run.code, one.name).toBe(verdict === "FAIL" ? 1 : 2);
      }
    },
    rowLimit({ atOnce: ["outsideEdgePageWait", "outsideEdgePageWait", "outsideEdgePageWait"] })
  );

  realTimeBrowserRow(
    "the relay step after a device login that passed: a listener nothing answers, a trade the broker never answers and an open still waiting at the bound are UNKNOWN, never FAIL",
    async () => {
      // The stand-in signs in, lists OWN and passes the device login. Each case's relay socket
      // then waits out the driver's 30 s: at a port nothing takes, after a token the stand-in
      // traded at once; at a stand-in whose trade for the session's token is never answered; and
      // at a listener that takes the connection and never answers, after a trade answered 12 s
      // late, so that at 30 s the upgrade still waits on its own 20 s and no open has failed. The
      // relay client keeps a code for each (`RelayNotOpenedError`): the refused connection's, the
      // trade's ETIMEDOUT, and ETIMEDOUT for the open still waiting, which is what the row reads.
      const silent = Bun.listen({
        hostname: "127.0.0.1",
        port: 0,
        socket: { data: () => undefined },
      });
      try {
        const signsIn = { enroll: "signs-in" } as const;
        const ran = await runCases(
          [
            ["relay-unanswered", signsIn],
            ["relay-trade-silent", { ...signsIn, exchange: "silent" }],
            [
              "relay-open-waits",
              { ...signsIn, exchange: 12_000 },
              undefined,
              `http://127.0.0.1:${silent.port}`,
            ],
          ],
          "outsideEdgePageWait"
        );
        const socket =
          "the relay socket did not open: the listener's relay socket for outside-e2e-[0-9a-f]{12} did not open";
        const wants: Record<string, RegExp> = {
          // A trade that failed would name its token instead ("its token could not be had"), so
          // this text also says the trade passed.
          "relay-unanswered": new RegExp(
            `^${socket}: the socket closed before it opened \\(1006: Failed to connect\\)$`
          ),
          // The relay asks for the token again 1 s after its 20 s wait ended; by the bound the
          // token client either still waits on the trade or has ended it at its own 30 s.
          "relay-trade-silent": new RegExp(
            `^${socket}: its token could not be had \\(the secrets broker (has not yet answered|did not answer) the trade of `
          ),
          "relay-open-waits": new RegExp(
            `^${socket}: its open was still waiting for its token or its upgrade's answer when the bound ended$`
          ),
        };
        for (const one of ran) {
          for (const row of ["sign-in", "projects", "device-login"]) {
            expect(rowOf(one, row).verdict, `${one.name} ${row}: ${one.run.stdout}`).toBe("PASS");
          }
          expect(rowOf(one, "relay").got, one.name).toMatch(wants[one.name] ?? /^$/);
          expect(rowOf(one, "relay").verdict, `${one.name}: ${one.run.stdout}`).toBe("UNKNOWN");
          expect(verdictLine(one.run), one.name).toStartWith("UNKNOWN: relay");
          expect(one.run.code, one.name).toBe(2);
        }
      } finally {
        silent.stop(true);
      }
    },
    rowLimit({ atOnce: ["outsideEdgePageWait", "outsideEdgePageWait", "outsideEdgePageWait"] })
  );

  /** A broker and a listener on envoy-client's relay socket stand-in, the edge a run meets after an
   *  origin stand-in's sign-in: it passes the device login, whose page is `page()`, the origin
   *  stand-in's; it trades the session's token for a subject token that login issued; and it
   *  answers each registration whose credential and topics are the session's own with
   *  `registered(session)`. Each unregister is kept, in order. */
  const startSessionEdge = (
    page: () => string,
    registered: (session: string) => Promise<Response>
  ) => {
    const unregisters: string[] = [];
    const issued = new Set<string>();
    const edge = startRelaySocketStandIn({
      routes: async (request, standIn) => {
        const { pathname } = new URL(request.url);
        if (request.method === "POST" && pathname === "/v1/oauth/device_authorization") {
          return Response.json({
            device_code: `own-device-${randomBytes(8).toString("hex")}`,
            user_code: "OWNS-CODE",
            verification_uri: `${page()}/credentials/machine`,
            expires_in: 600,
            interval: 1,
          });
        }
        if (request.method === "POST" && pathname === "/v1/oauth/token") {
          const form = new URLSearchParams(await request.text());
          if (form.get("grant_type") !== "urn:ietf:params:oauth:grant-type:device_code") {
            // The exchange: the stand-in's own trade mints the session's token, for a subject
            // token the device grant issued.
            return tradeRefusal(form, issued);
          }
          const accessToken = `own-login-${randomBytes(16).toString("hex")}`;
          issued.add(accessToken);
          return Response.json({
            access_token: accessToken,
            token_type: "DPoP",
            expires_in: 900,
            refresh_token: `own-refresh-${randomBytes(16).toString("hex")}`,
            scope: "machine",
            credential_id: standInCredential,
            credential_expires_at: new Date(Date.now() + 86_400_000)
              .toISOString()
              .replace(/\.\d{3}Z$/, "Z"),
          });
        }
        if (request.method === "POST" && pathname === "/v1/interests/subscribe") {
          const body: unknown = await request.json();
          const field = (name: string) =>
            typeof body === "object" && body !== null ? Reflect.get(body, name) : undefined;
          const session = String(field("session_id"));
          const refusal =
            (await sessionRefusal(request, standIn, session)) ??
            topicRefusal(session, field("topics"));
          return refusal ?? (await registered(session));
        }
        const ended = /^\/v1\/sessions\/([^/]+)$/.exec(pathname);
        if (request.method === "DELETE" && ended !== null) {
          unregisters.push(decodeURIComponent(ended[1] ?? ""));
          return Response.json({ deleted: true });
        }
        return undefined;
      },
    });
    return { edge, unregisters };
  };

  browserRow(
    "an error of the driver's own in step 2 or 3, a login or a session it cannot keep in OUTSIDE_EDGE_STATE, is UNKNOWN, exit 2, never FAIL, and leaves no copy of the login behind",
    async () => {
      // Step 2: the state directory holds a directory where the device login is kept, so the
      // broker client refuses to save the login the page approved after the broker answered. And
      // it holds a whole copy of a login as the broker client keeps one beside that file where a
      // save fails after its temporary copy is whole (`device-login.json.kept-<uuid>`, which holds
      // the login's key and refresh token), which the driver must remove. The broker client keeps
      // one only where the rename over the file fails, or its lock is taken, after that copy was
      // synced, which no row outside its process can bring about on cue, so the row lays one
      // down as an earlier run of the same state would have left it.
      const loginUnkept = "own-login-unkept";
      mkdirSync(join(scratch, loginUnkept, "device-login.json"), { recursive: true, mode: 0o700 });
      writeFileSync(
        join(scratch, loginUnkept, `device-login.json.kept-${crypto.randomUUID()}`),
        JSON.stringify({
          key: `kept-key-${randomBytes(8).toString("hex")}`,
          refresh_token: `kept-refresh-${randomBytes(8).toString("hex")}`,
        }),
        { mode: 0o600 }
      );
      // Step 3: the session edge, whose registration is answered only once the row has made
      // `session` a directory, so the driver's write of the session the listener took fails.
      let page = "";
      let registered: string | undefined;
      const answer = Promise.withResolvers<void>();
      const { edge, unregisters } = startSessionEdge(
        () => page,
        async (session) => {
          registered = session;
          await answer.promise;
          return Response.json({
            session_id: session,
            machine_id: "outside-edge-own-stand-in",
            dir: "/outside-edge-e2e",
            topics: [legacyAgentSubject(session)],
            delivery: consumerDelivery(session),
          });
        }
      );
      const signsIn = { enroll: "signs-in" } as const;
      const login = startRun(loginUnkept, signsIn, "outsideEdgeBrowser");
      const session = startRun("own-session-unkept", signsIn, "short", edge.url, edge.url);
      page = session.origin.page;
      try {
        await eventually("the registration at the listener", () => registered !== undefined);
        const took = registered ?? "";
        mkdirSync(join(session.state, "session", "kept-elsewhere"), { recursive: true });
        answer.resolve();
        const [loginRun, sessionRun] = await Promise.all([login.ran, session.ran]);
        const loginRows = rowsOf(loginRun);
        expect(loginRows.get("device-login"), loginRun.stdout).toEqual({
          want: expect.any(String),
          got: `approved on the page as credential ${standInCredential}, and the poll answered, but the driver could not keep the login in OUTSIDE_EDGE_STATE's device-login.json, an error of its own; it removed the copy of a login the broker client kept beside that file; revoke ends credential ${standInCredential}`,
          verdict: "UNKNOWN",
        });
        // No copy of the login, nor a save's temporary one, is left in the state; the credential
        // is recorded, for `revoke` to end.
        const left = readdirSync(login.state).filter((name) =>
          /^device-login\.json\.kept-|^\.device-login-.*\.tmp$/.test(name)
        );
        expect(left).toEqual([]);
        expect(readFileSync(join(login.state, "approved.tsv"), "utf8")).toBe(
          `STND-CODE\t${standInCredential}\n`
        );
        expect(loginRows.get("relay")).toMatchObject({
          got: "not run: the device login did not pass",
          verdict: "UNKNOWN",
        });
        expect(verdictLine(loginRun)).toStartWith("UNKNOWN: device-login");
        expect(loginRun.code).toBe(2);
        const sessionRows = rowsOf(sessionRun);
        expect(sessionRows.get("device-login")?.verdict, sessionRun.stdout).toBe("PASS");
        expect(sessionRows.get("relay")).toMatchObject({
          got: `the listener took ${took}, and the driver could not keep it, an error of its own: open EISDIR on OUTSIDE_EDGE_STATE's session`,
          verdict: "UNKNOWN",
        });
        expect(verdictLine(sessionRun)).toStartWith("UNKNOWN: relay");
        expect(sessionRun.code).toBe(2);
        // The session the listener took is ended, as at every end of the step.
        expect(unregisters).toEqual([took]);
      } finally {
        answer.resolve();
        login.origin.stop();
        session.origin.stop();
        edge.stop();
      }
    },
    rowLimit({ atOnce: ["outsideEdgeBrowser", "short"] })
  );

  browserRow(
    "an approved code the driver cannot record is named on stderr first, and run and approve read UNKNOWN, exit 2, naming the state's file",
    async () => {
      // `approved.tsv` is a directory in each state, so the record of the code the page approved
      // fails: the credential is live and in no approved.tsv, and only the output names it.
      mkdirSync(join(scratch, "unrecorded-run", "approved.tsv"), { recursive: true, mode: 0o700 });
      const unrecorded = startRun("unrecorded-run", { enroll: "signs-in" }, "outsideEdgeBrowser");
      const origin = startOriginStandIn();
      const approveState = stateWith({
        origin: origin.url,
        person: standInPerson,
        "browser.json": JSON.stringify({ cookies: [], origins: [] }),
      });
      mkdirSync(join(approveState, "approved.tsv"));
      try {
        const [run, approved] = await Promise.all([
          unrecorded.ran,
          drive(
            ["approve", "ABCD-EFGH"],
            { OUTSIDE_EDGE_STATE: approveState },
            "outsideEdgeBrowser"
          ),
        ]);
        expect(run.stderr, run.stdout).toContain(
          `outside-edge: STND-CODE approved as credential ${standInCredential}\n`
        );
        expect(rowsOf(run).get("stopped"), run.stdout).toEqual({
          want: "every step judged",
          got: "the run stopped: open EISDIR on OUTSIDE_EDGE_STATE's approved.tsv",
          verdict: "UNKNOWN",
        });
        expect(run.code).toBe(2);
        expect(approved.stderr, approved.stdout).toContain(
          `outside-edge: ABCD-EFGH approved as credential ${standInCredential}\n`
        );
        expect(approved.stdout).toBe(
          "UNKNOWN: approve failed on an error of its own: open EISDIR on OUTSIDE_EDGE_STATE's approved.tsv\n"
        );
        expect(approved.code).toBe(2);
      } finally {
        unrecorded.origin.stop();
        origin.stop();
      }
    },
    rowLimit({ atOnce: ["outsideEdgeBrowser", "outsideEdgeBrowser"] })
  );

  browserRow(
    "the relay step reads the listener's refusal as FAIL, even where its body names a code of a request that got no answer",
    async () => {
      // The listener answers the registration 400 with the code ETIMEDOUT: text it wrote
      // (`EnvoyApiError.code`), so the refusal is its answer, never a far end that answered nothing.
      let page = "";
      const { edge } = startSessionEdge(
        () => page,
        async () =>
          Response.json(
            { error: "the stand-in refuses this registration", code: "ETIMEDOUT" },
            { status: 400 }
          )
      );
      const refused = startRun(
        "relay-refusal-code",
        { enroll: "signs-in" },
        "outsideEdgeBrowser",
        edge.url,
        edge.url
      );
      page = refused.origin.page;
      try {
        const run = await refused.ran;
        const relayRow = rowsOf(run).get("relay");
        expect(relayRow?.got, run.stdout).toBe(
          "the session could not register or send: the stand-in refuses this registration"
        );
        expect(relayRow?.verdict).toBe("FAIL");
        expect(verdictLine(run)).toStartWith("FAIL: relay");
        expect(run.code).toBe(1);
      } finally {
        refused.origin.stop();
        edge.stop();
      }
    },
    rowLimit("outsideEdgeBrowser")
  );

  realTimeBrowserRow(
    "the relay step reads a registration answered with a status whose body never comes as FAIL, a 503 or a 200: the listener answered",
    async () => {
      // The listener sends the registration's status and the start of its body, and never the
      // rest. The Envoy client waits out its deadline for the body on each attempt and names the
      // status that came, with no code of a request that got no answer: the status is the
      // listener's answer. So the step proved something, a FAIL: for a refusal (503), and for a
      // 200 whose registration the session never got to read, an answer short of the pong.
      const stalled = (status: number) =>
        new Response(
          new ReadableStream({
            start: (controller) => controller.enqueue(new TextEncoder().encode('{"error":')),
          }),
          { status, headers: { "Content-Type": "application/json" } }
        );
      const started = [503, 200].map((status) => {
        let page = "";
        const { edge } = startSessionEdge(
          () => page,
          async () => stalled(status)
        );
        const run = startRun(
          `relay-stalled-${status}`,
          { enroll: "signs-in" },
          "outsideEdgeBrowser",
          edge.url,
          edge.url
        );
        page = run.origin.page;
        return { status, edge, run };
      });
      try {
        for (const { status, run } of started) {
          const ran = await run.ran;
          const relayRow = rowsOf(ran).get("relay");
          expect(relayRow?.got, `${status}: ${ran.stdout}`).toMatch(
            new RegExp(
              `^the session could not register or send: POST http://127\\.0\\.0\\.1:[0-9]+/v1/interests/subscribe answered ${status}, and its body did not arrive: TimeoutError \\(message withheld\\)$`
            )
          );
          expect(relayRow?.verdict, `${status}: ${ran.stdout}`).toBe("FAIL");
          expect(verdictLine(ran), String(status)).toStartWith("FAIL: relay");
          expect(ran.code, String(status)).toBe(1);
        }
      } finally {
        for (const { edge, run } of started) {
          run.origin.stop();
          edge.stop();
        }
      }
    },
    rowLimit({ atOnce: ["outsideEdgeBrowser", "outsideEdgeBrowser"] })
  );

  realTimeBrowserRow(
    "a browser that ends while a page step waits is UNKNOWN, the driver's own failure, never a page that showed nothing",
    async () => {
      // The enrollment's Create a passkey tells the stand-in it was pressed and does nothing more,
      // so the driver then waits up to 30 s for a sign-in or a refusal (`settledWithin`); its
      // Chromium is ended inside that wait.
      const started = startRun("page-gone", { enroll: "no-sign-in" }, "outsideEdgeBrowser");
      try {
        await eventually("the press of Create a passkey", () =>
          started.origin.asked.includes("/__clicked")
        );
        // A fixed wait, inside the driver's own 30 s one: the press reaches the stand-in just
        // before the driver's click returns and its wait begins, which nothing here can see.
        await Bun.sleep(3_000);
        expect(endBrowserOf(started.state), "Chromium of the run, running").toBeGreaterThan(0);
        const run = await started.ran;
        expect(rowsOf(run).get("sign-in"), run.stdout).toBeUndefined();
        expect(rowsOf(run).get("stopped")?.got).toMatch(
          /^the run stopped: the (page closed|browser ended) while the driver read the enrollment after Create a passkey$/
        );
        expect(rowsOf(run).get("stopped")?.verdict).toBe("UNKNOWN");
        expect(run.code).toBe(2);
      } finally {
        started.origin.stop();
      }
    },
    rowLimit("short", "outsideEdgeBrowser")
  );

  realTimeBrowserRow(
    "a browser that ends while the machine-login page's code box or credential is awaited is UNKNOWN, the driver's own failure, never a page that showed nothing",
    async () => {
      // The machine-login page shows no code box, or says Approved and names no credential, so
      // the driver waits up to 30 s for the box, or for the credential's line; its Chromium is
      // ended inside that wait. The wait begins inside the driver, where nothing here can see it,
      // just after the page is asked the path that precedes it (its load, or the approval's
      // request), so the row ends the browser a fixed 3 s after that path, well inside the 30 s.
      const cases = [
        ["gone-no-box", "no-box", "/credentials/machine", "the machine-login page's code box"],
        [
          "gone-no-credential",
          "no-credential",
          "/__approve",
          "the credential the machine-login page named once it said Approved",
        ],
      ] as const;
      const started = cases.map(([name, machine, asked, read]) => ({
        name,
        asked,
        read,
        ...startRun(name, { enroll: "signs-in", machine }, "outsideEdgeBrowser"),
      }));
      try {
        await Promise.all(
          started.map(async (one) => {
            await eventually(`${one.name}: the page's ${one.asked}`, () =>
              one.origin.asked.includes(one.asked)
            );
            await Bun.sleep(3_000);
            expect(endBrowserOf(one.state), `${one.name}: Chromium, running`).toBeGreaterThan(0);
          })
        );
        for (const one of started) {
          const run = await one.ran;
          const login = rowsOf(run).get("device-login");
          expect(login?.got, `${one.name}: ${run.stdout}`).toMatch(
            new RegExp(
              `^the machine-login page could not be driven: the (page closed|browser ended) while the driver read ${one.read}$`
            )
          );
          expect(login?.verdict, one.name).toBe("UNKNOWN");
          expect(verdictLine(run), one.name).toStartWith("UNKNOWN: device-login");
          expect(run.code, one.name).toBe(2);
        }
      } finally {
        for (const one of started) one.origin.stop();
      }
    },
    rowLimit("short", { atOnce: ["outsideEdgeBrowser", "outsideEdgeBrowser"] })
  );

  realTimeBrowserRow(
    "a page that holds its main thread while the driver reads through it, or saves the browser's state, is UNKNOWN at the driver's own bound, naming what it held",
    async () => {
      // The signed-in page holds its main thread: its `fetch` loops forever, so the driver's read
      // of `GET /auth/whoami` through the page never returns (`read`); or it loops once its
      // second read, the project list, has handed back its answer, so the driver's save of the
      // browser's state, which reads the page's storage through a script in the page, never
      // returns (`after-reads`). Playwright has no deadline for either, nor for the browser's
      // close, which waits on such a page, and the page runs no deadline of its own, so only the
      // driver's own bounds give the run a verdict. Each run's Chromium, read once its page signed
      // the person in, is gone once the run ended: killed as the driver exits where its close
      // outlasted the driver's bound.
      const cases = [
        ["held-read", { enroll: "signs-in", blocks: "read" }],
        ["held-save", { enroll: "signs-in", blocks: "after-reads" }],
      ] as const;
      const started = cases.map(([name, behaviour]) => ({
        name,
        ...startRun(name, behaviour, "outsideEdgePageWait"),
      }));
      try {
        const browsers = await Promise.all(
          started.map(async (one) => {
            await eventually(`${one.name}: the sign-in`, () =>
              one.origin.asked.includes("/__sign-in")
            );
            return browserOf(one.state);
          })
        );
        const [read, save] = await Promise.all(
          started.map(async (one) => {
            const run = await one.ran;
            return { name: one.name, run, rows: rowsOf(run) };
          })
        );
        if (read === undefined || save === undefined) throw new Error("a case did not run");
        expect(rowOf(read, "sign-in"), read.run.stdout).toMatchObject({
          got: "GET /auth/whoami got no answer: the page's script held the read past 35 s",
          verdict: "UNKNOWN",
        });
        expect(verdictLine(read.run)).toStartWith("UNKNOWN: sign-in");
        expect(read.run.code).toBe(2);
        for (const row of ["sign-in", "projects"]) {
          expect(rowOf(save, row).verdict, `${row}: ${save.run.stdout}`).toBe("PASS");
        }
        expect(rowOf(save, "stopped")).toEqual({
          want: "every step judged",
          got: "the run stopped: the save of the browser's signed-in state did not end within 30 s, so the run kept none",
          verdict: "UNKNOWN",
        });
        expect(existsSync(join(scratch, "held-save", "browser.json"))).toBe(false);
        expect(save.run.code).toBe(2);
        for (const [at, browser] of browsers.entries()) {
          const name = started[at]?.name;
          expect(browser.length, `${name}: Chromium, running`).toBeGreaterThan(0);
          await eventually(`${name}: its Chromium gone`, () =>
            browser.every((one) => !running(one.pid))
          );
        }
      } finally {
        for (const { origin } of started) origin.stop();
      }
    },
    rowLimit("short", { atOnce: ["outsideEdgePageWait", "outsideEdgePageWait"] }, "short")
  );

  browserRow(
    "approve reads UNKNOWN, exit 2, where the machine-login page cannot be driven, and prints a refusal on one line",
    async () => {
      const untypable = startOriginStandIn({ machine: "untypable" });
      // A refusal on two lines, the second `APPROVED ABCD-EFGH`: approve's output must hold no line
      // that reads as an approval.
      const lines = startOriginStandIn({ machine: "refuses-code-lines" });
      const approveAt = (origin: OriginStandIn) =>
        drive(
          ["approve", "ABCD-EFGH"],
          {
            OUTSIDE_EDGE_STATE: stateWith({
              origin: origin.url,
              person: standInPerson,
              "browser.json": JSON.stringify({ cookies: [], origins: [] }),
            }),
          },
          "outsideEdgeBrowser"
        );
      try {
        const [undriven, refused] = await Promise.all([approveAt(untypable), approveAt(lines)]);
        expect(undriven.stdout, undriven.stderr).toStartWith(
          "UNKNOWN: ABCD-EFGH not approved: the machine-login page could not be driven: "
        );
        expect(undriven.stdout.trimEnd().split("\n")).toHaveLength(1);
        expect(undriven.code).toBe(2);
        expect(refused.stdout, refused.stderr).toBe(
          "FAIL: ABCD-EFGH not approved: the page refused the code: No machine login waits on that code. APPROVED ABCD-EFGH\n"
        );
        expect(refused.stdout.split("\n")).not.toContain("APPROVED ABCD-EFGH");
        expect(refused.code).toBe(1);
      } finally {
        untypable.stop();
        lines.stop();
      }
    },
    rowLimit({ atOnce: ["outsideEdgeBrowser", "outsideEdgeBrowser"] })
  );
});

describe("the fake broker the stack's Dispatch relays the machine-login page to", () => {
  test(
    "its revoke of a machine login answers as the broker's: 403 NOT_APPROVER for another person's, 404 NOT_FOUND for no login, 200 for the person's own",
    async () => {
      const broker = Bun.spawn(["bun", join(dispatchPackage, "e2e/fake-broker.ts")], {
        cwd: dispatchPackage,
        env: { PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "" },
        stdout: "pipe",
        stderr: "pipe",
      });
      try {
        // The port it bound, which its one line of output names.
        const reader = broker.stdout.getReader();
        let said = "";
        const deadline = performance.now() + 60_000;
        while (!/listener on 127\.0\.0\.1:[0-9]+\n/.test(said)) {
          const read = await beforeDeadline(reader.read(), deadline);
          if (read === pastDeadline || read.done) throw new Error(`the fake broker said: ${said}`);
          said += new TextDecoder().decode(read.value);
        }
        const origin = `http://${/listener on (127\.0\.0\.1:[0-9]+)/.exec(said)?.[1]}`;
        const seeded = await fetch(`${origin}/__fixture/launcher-credentials`, {
          method: "PUT",
          body: JSON.stringify([
            {
              approved_by: standInPerson,
              credential_id: "cred-carol",
              expired: false,
              expires_at: new Date(Date.now() + 86_400_000).toISOString(),
              host: "outside-edge-e2e",
              issued_at: new Date().toISOString(),
              person: null,
              service: null,
              subject: null,
              listedFor: standInPerson,
            },
          ]),
        });
        expect(seeded.status).toBe(200);
        const revoke = async (id: string, approver: string) => {
          const answer = await fetch(`${origin}/v1/launcher-credentials/${id}/revoke-by-approver`, {
            method: "POST",
            body: JSON.stringify({ approver }),
          });
          return { status: answer.status, body: (await answer.json()) as Record<string, unknown> };
        };
        const listed = async () => {
          const answer = await fetch(
            `${origin}/v1/launcher-credentials?${new URLSearchParams({ approver: standInPerson })}`
          );
          const body = (await answer.json()) as { credentials: { credential_id: string }[] };
          return body.credentials.map((one) => one.credential_id);
        };
        expect(await revoke("cred-carol", "mallory@outside.example")).toMatchObject({
          status: 403,
          body: { code: "NOT_APPROVER" },
        });
        expect(await listed()).toEqual(["cred-carol"]);
        expect(await revoke("cred-none", standInPerson)).toMatchObject({
          status: 404,
          body: { code: "NOT_FOUND" },
        });
        expect(await revoke("cred-carol", standInPerson)).toEqual({
          status: 200,
          body: { state: "revoked" },
        });
        expect(await listed()).toEqual([]);
      } finally {
        broker.kill();
        await broker.exited;
      }
    },
    rowLimit("short")
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

  stackRow(
    "run keeps exactly the session the listener took, mode 0600, and hold and release hold it and end it",
    () => {
      // The one registration of the run, which `session` must name, and nothing else.
      expect(runs.passSession.registered, runs.pass.stderr).toHaveLength(1);
      const session = runs.passSession.registered[0];
      expect(session).toMatch(/^outside-e2e-[0-9a-f]{12}$/);
      expect(runs.passSession.kept).toBe(session);
      expect(runs.passSession.mode).toBe(0o600);
      expect(runs.hold.stdout, runs.hold.stderr).toBe(`HELD ${session}\nRELEASED ${session}\n`);
      expect(runs.hold.code).toBe(0);
      expect(runs.registeredWhileHeld).toBe(true);
      expect(runs.release.stdout, runs.release.stderr).toBe(`RELEASED ${session}\n`);
      expect(runs.release.code).toBe(0);
      expect(runs.registeredAfterRelease).toBe(false);
      // The run's own end of that registration, printed with what the listener answered.
      expect(runs.pass.stdout).toContain(`\noutside-edge: session ${session}: unregistered\n`);
    }
  );

  stackRow(
    "every registration names the device login's machine name, never this machine's, and the edge refused no credential or topic",
    () => {
      expect(runs.registrationHosts.length).toBeGreaterThan(0);
      expect(new Set(runs.registrationHosts)).toEqual(new Set(["outside-edge-e2e"]));
      expect(runs.registrationHosts).not.toContain(hostname());
      expect(runs.edgeRefusals).toEqual([]);
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
