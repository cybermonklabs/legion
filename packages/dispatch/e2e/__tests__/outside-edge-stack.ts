/** `bun e2e/__tests__/outside-edge-stack.ts`, from packages/dispatch with
 *  `DISPATCH_E2E_SIGNIN=google`: one stack of the e2e harness (`e2e/harness-pool.ts`, whose
 *  Dispatch server `e2e/run-server.sh` starts with passkey sign-in on at the outside origin
 *  `http://localhost:<port>`), the edge stand-in (`outside-edge-stand-in.ts`), and the outside
 *  driver (`e2e/acceptance/outside-edge.ts`) run against them in each of the cases below. It
 *  prints one JSON line, what each run answered (`StackRuns`), and stops the stack however it
 *  ends. `outside-edge.test.ts` runs it and reads that line.
 *
 *  The owner, alice of the sign-in domain, signs in at the canonical origin through the dev
 *  issuer, makes OWN and OTHER, lists each person on the projects their case needs, and makes
 *  each person's link (`POST /api/v1/projects/{key}/access/people/{email}/invitations`). Each
 *  case spends its own link, so each has a person of its own:
 *  - `pass`: carol, on OWN alone, with a second pending code of hers as the tailnet leg's; then
 *    `hold` of the session her run kept, ended by `release`, as a check that acts as her session
 *    holds it; then `approve` of a third code of hers, `revoke`, and `t5` twice: while she is
 *    listed, and after the owner took her off OWN.
 *  - `secondProject`: bob, on OWN and OTHER: the run must fail step 1 naming OTHER.
 *  - `silentPeer`: dave, on OWN alone, sending to a peer that never answers: step 3 must fail by
 *    timeout, naming that peer. Then `t5` with his kept passkey and a cookie Dispatch refuses,
 *    while he is still listed: the passkey must sign him in, the positive control of carol's
 *    refused one.
 *  - `oneRequest`: erin, on OWN alone, with the listener's threshold of a minute set to 1: step 4
 *    must fail, naming the host.
 *  - `signIn`: frank, on OWN alone, the sign-in steps alone (`OUTSIDE_EDGE_STEPS=sign-in`), with no
 *    peer, broker or listener: sign-in and projects must pass, and step 4 judge the outside host's
 *    rules alone, naming every other row not run.
 */

import {
  copyFileSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { type BrowserContext, chromium } from "@playwright/test";
import { startStacks, stopStacks } from "../harness-pool";
import { googleOutsideOrigin, googleSignIn } from "../harness-signin";
import { startEdgeStandIn } from "./outside-edge-stand-in";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const driver = join(dispatchPackage, "e2e/acceptance/outside-edge.ts");

const alice = `alice@${googleSignIn.domain}`;
export const people = {
  pass: "carol@outside.example",
  secondProject: "bob@outside.example",
  silentPeer: "dave@outside.example",
  oneRequest: "erin@outside.example",
  signIn: "frank@outside.example",
} as const;
export const peer = "ses-outside-peer";
export const silentPeer = "ses-outside-silent";

/** What one run of the driver answered. */
export interface DriverRun {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** What the child prints. */
export interface StackRuns {
  readonly pass: DriverRun;
  /** The session `pass` kept (`<state>/session`) and its mode, null where it kept none, and the
   *  session each registration the edge took during that run named. */
  readonly passSession: {
    readonly kept: string | null;
    readonly mode: number | null;
    readonly registered: readonly string[];
  };
  /** `hold` of that session and the `release` that ended it, and whether the edge held the
   *  session registered while it was held and after the release. */
  readonly hold: DriverRun;
  readonly release: DriverRun;
  readonly registeredWhileHeld: boolean;
  readonly registeredAfterRelease: boolean;
  readonly tailnetCode: string;
  readonly approve: DriverRun;
  readonly approveCode: string;
  readonly revoke: DriverRun;
  /** approve's credential, which the revoke keeps, and what the person's list holds after it. */
  readonly keptCredential: string;
  readonly listedAfterRevoke: readonly string[];
  /** `t5` while the person is still listed (its deadline comes first), then after the owner took
   *  them off OWN, and dave's, still listed, with a refused cookie, each with the TSV it wrote. */
  readonly t5Listed: DriverRun & { readonly tsv: string };
  readonly t5Delisted: DriverRun & { readonly tsv: string };
  readonly t5StillListed: DriverRun & { readonly tsv: string };
  readonly secondProject: DriverRun;
  readonly silentPeer: DriverRun;
  readonly oneRequest: DriverRun;
  /** The sign-in steps alone, given no peer, broker or listener. */
  readonly signIn: DriverRun;
  /** The edge's two hosts of this stack, as the limits file names them. */
  readonly outsideHost: string;
  readonly edgeHost: string;
  /** The `host` each registration the edge took named, in order, and each request a listener
   *  route or the exchange refused for its credential or its topics. */
  readonly registrationHosts: readonly string[];
  readonly edgeRefusals: readonly string[];
}

/** The rate rules of this stack, in the edge's shape: the broker's two exact paths and the
 *  listener's prefix on the edge stand-in's host, and Dispatch's sign-in prefix and catch-all on
 *  the outside origin's. `oneRequest` sets the listener's minute to 1. */
function limits(edgeHost: string, outsideHost: string, listenerPerMinute = 600): string {
  return [
    `1000 ${edgeHost} exact /v1/oauth/device_authorization 20 60`,
    `1000 ${edgeHost} exact /v1/oauth/device_authorization 100 600`,
    `1010 ${edgeHost} exact /v1/oauth/token 240 60`,
    `1010 ${edgeHost} exact /v1/oauth/token 2400 600`,
    `2000 ${edgeHost} prefix /v1/ ${listenerPerMinute} 60`,
    `2000 ${edgeHost} prefix /v1/ 6000 600`,
    `3000 ${outsideHost} decoded-prefix /auth/ 60 60`,
    `3000 ${outsideHost} decoded-prefix /auth/ 600 600`,
    `3010 ${outsideHost} any - 1200 60`,
    `3010 ${outsideHost} any - 12000 600`,
    "",
  ].join("\n");
}

/** Signs alice in at `canonical` through the issuer, as the sign-in page's Google link does. */
async function ownerSignsIn(
  context: BrowserContext,
  canonical: string,
  issuer: string
): Promise<void> {
  const queued = await fetch(`${issuer}/_dev/sign-in-as`, {
    method: "POST",
    body: JSON.stringify({
      email: alice,
      email_verified: true,
      hd: googleSignIn.domain,
      sub: alice,
    }),
  });
  if (queued.status !== 204) throw new Error(`the issuer did not queue ${alice}: ${queued.status}`);
  const page = await context.newPage();
  await page.goto(`${canonical}/`);
  await page.getByRole("link", { name: "Sign in with Google" }).click();
  await page.getByText(`Signed in as ${alice}`).waitFor({ timeout: 30_000 });
  await page.close();
}

/** One write alice makes with her cookie at the canonical origin, which must succeed. */
async function ownerWrites(
  context: BrowserContext,
  canonical: string,
  method: "DELETE" | "POST" | "PUT",
  path: string,
  data?: object
): Promise<unknown> {
  const answer = await context.request.fetch(`${canonical}${path}`, {
    method,
    data,
    headers: { Origin: canonical },
  });
  if (!answer.ok()) throw new Error(`${method} ${path}: ${answer.status()} ${await answer.text()}`);
  const text = await answer.text();
  return text === "" ? undefined : JSON.parse(text);
}

/** Runs the driver with `args` and these variables alone, beside PATH and HOME. */
async function drive(args: readonly string[], env: Record<string, string>): Promise<DriverRun> {
  const child = Bun.spawn(["bun", driver, ...args], {
    cwd: dispatchPackage,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: process.env.HOME ?? "",
      ...(process.env.PLAYWRIGHT_BROWSERS_PATH === undefined
        ? {}
        : { PLAYWRIGHT_BROWSERS_PATH: process.env.PLAYWRIGHT_BROWSERS_PATH }),
      ...env,
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { code, stdout, stderr };
}

/** `hold` of the session `run` kept in `state`, started as a check starts it, with
 *  OUTSIDE_PROJECT, the edge's two URLs and OUTSIDE_EDGE_STATE alone (and
 *  OUTSIDE_EDGE_ALLOW_HTTP), and `release` once its `held` is there, as the check polls for it
 *  for 60 s; and whether `registered` held the kept session while it was held and after. */
async function holdAndRelease(
  state: string,
  edge: string,
  registered: (session: string) => boolean
): Promise<Pick<StackRuns, "hold" | "release" | "registeredWhileHeld" | "registeredAfterRelease">> {
  const holding = Bun.spawn(["bun", driver, "hold"], {
    cwd: dispatchPackage,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: process.env.HOME ?? "",
      OUTSIDE_EDGE_ALLOW_HTTP: "1",
      OUTSIDE_PROJECT: "OWN",
      OUTSIDE_BROKER_URL: edge,
      OUTSIDE_LISTENER_URL: edge,
      OUTSIDE_EDGE_STATE: state,
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  const ended = Promise.all([
    holding.exited,
    new Response(holding.stdout).text(),
    new Response(holding.stderr).text(),
  ]);
  try {
    const held = join(state, "held");
    const until = Date.now() + 60_000;
    while (!existsSync(held) && holding.exitCode === null && Date.now() < until) {
      await Bun.sleep(100);
    }
    const session = existsSync(join(state, "session"))
      ? readFileSync(join(state, "session"), "utf8")
      : "";
    const registeredWhileHeld = existsSync(held) && registered(session);
    const release = await drive(["release"], { OUTSIDE_EDGE_STATE: state });
    // The hold ends at release's SIGTERM; one that does not is ended here, and its run says so.
    const late = setTimeout(() => holding.kill("SIGKILL"), 20_000);
    const [code, stdout, stderr] = await ended;
    clearTimeout(late);
    return {
      hold: { code, stdout, stderr },
      release,
      registeredWhileHeld,
      registeredAfterRelease: registered(session),
    };
  } finally {
    if (holding.exitCode === null) holding.kill("SIGKILL");
  }
}

if (import.meta.main) {
  const scratch = mkdtempSync(join(tmpdir(), "outside-edge-stack-"));
  try {
    const [stack] = await startStacks(1);
    try {
      if (stack === undefined) throw new Error("the harness started no stack");
      const port = stack.env.DISPATCH_E2E_PORT;
      const issuerPort = stack.env.DISPATCH_E2E_SIGNIN_ISSUER_PORT;
      const brokerPort = stack.env.FAKE_BROKER_PORT;
      if (port === undefined || issuerPort === undefined || brokerPort === undefined) {
        throw new Error("the stack names no Dispatch, sign-in issuer or fake broker port");
      }
      const canonical = `http://127.0.0.1:${port}`;
      const outside = googleOutsideOrigin(canonical);
      const edge = startEdgeStandIn({
        fakeBroker: `http://127.0.0.1:${brokerPort}`,
        outsideOrigin: outside,
        peer,
        silentPeer,
      });
      const browser = await chromium.launch();
      try {
        const owner = await browser.newContext();
        await ownerSignsIn(owner, canonical, `http://127.0.0.1:${issuerPort}`);
        for (const key of ["OWN", "OTHER"]) {
          await ownerWrites(owner, canonical, "POST", "/api/v1/projects", {
            key,
            name: `${key} work`,
          });
        }
        const lists: Record<string, readonly string[]> = {
          [people.pass]: ["OWN"],
          [people.secondProject]: ["OWN", "OTHER"],
          [people.silentPeer]: ["OWN"],
          [people.oneRequest]: ["OWN"],
          [people.signIn]: ["OWN"],
        };
        for (const [email, keys] of Object.entries(lists)) {
          for (const key of keys) {
            await ownerWrites(
              owner,
              canonical,
              "PUT",
              `/api/v1/projects/${key}/access/people/${email}`
            );
          }
        }
        const invite = async (email: string): Promise<string> => {
          const answer = await ownerWrites(
            owner,
            canonical,
            "POST",
            `/api/v1/projects/OWN/access/people/${email}/invitations`
          );
          const url =
            typeof answer === "object" && answer !== null ? Reflect.get(answer, "url") : undefined;
          if (typeof url !== "string")
            throw new Error(`the invitation of ${email} answered no url`);
          return url;
        };
        const edgeHost = new URL(edge.url).hostname;
        const outsideHost = new URL(outside).hostname;
        const limitsFile = join(scratch, "limits.txt");
        writeFileSync(limitsFile, limits(edgeHost, outsideHost));
        const oneRequestLimits = join(scratch, "one-request.txt");
        writeFileSync(oneRequestLimits, limits(edgeHost, outsideHost, 1));
        const run = async (
          name: keyof typeof people,
          env: Record<string, string> = {}
        ): Promise<DriverRun> =>
          drive([], {
            OUTSIDE_EDGE_ALLOW_HTTP: "1",
            OUTSIDE_INVITE_URL: await invite(people[name]),
            OUTSIDE_PROJECT: "OWN",
            OUTSIDE_PEER_SESSION: peer,
            OUTSIDE_BROKER_URL: edge.url,
            OUTSIDE_LISTENER_URL: edge.url,
            OUTSIDE_EDGE_LIMITS: limitsFile,
            OUTSIDE_EDGE_STATE: join(scratch, name),
            OUTSIDE_EDGE_REPLY_SECONDS: "5",
            ...env,
          });

        const tailnetCode = "TLNT-CODE";
        await edge.addMachineLogin(tailnetCode, people.pass);
        const registeredBefore = edge.registrations.length;
        const pass = await run("pass", { OUTSIDE_TAILNET_USER_CODE: tailnetCode });
        const sessionFile = join(scratch, "pass", "session");
        const passSession = {
          kept: existsSync(sessionFile) ? readFileSync(sessionFile, "utf8") : null,
          mode: existsSync(sessionFile) ? statSync(sessionFile).mode & 0o777 : null,
          registered: edge.registrations.slice(registeredBefore),
        };
        const held = await holdAndRelease(join(scratch, "pass"), edge.url, (session) =>
          edge.registered.has(session)
        );
        const approveCode = "MORE-CODE";
        await edge.addMachineLogin(approveCode, people.pass);
        const state = { OUTSIDE_EDGE_STATE: join(scratch, "pass") };
        const approve = await drive(["approve", approveCode], state);
        // The person's own live logins, as the broker lists them, which the revoke reads.
        await edge.seedApproved();
        // It keeps approve's login, as the public leg keeps P2's for its delisting window.
        const revoke = await drive(["revoke"], { ...state, OUTSIDE_EDGE_REVOKE_KEEP: approveCode });
        const keptCredential = (await edge.lookup(approveCode)).decided?.credential_id ?? "";
        const listedAfterRevoke = await edge.listed(people.pass);
        // The delisting window's browser half: first while the person is still on OWN, which its
        // deadline must end UNKNOWN, then after the owner takes them off it.
        const window = async (name: string, seconds: number, at = state) => {
          const tsv = join(scratch, `${name}.tsv`);
          const deadline = String(Math.floor(Date.now() / 1000) + seconds);
          const ran = await drive(["t5"], {
            ...at,
            OUTSIDE_T5_DEADLINE: deadline,
            OUTSIDE_T5_TSV: tsv,
          });
          return { ...ran, tsv: readFileSync(tsv, "utf8") };
        };
        const t5Listed = await window("t5-listed", 8);
        await ownerWrites(
          owner,
          canonical,
          "DELETE",
          `/api/v1/projects/OWN/access/people/${people.pass}`
        );
        const t5Delisted = await window("t5-delisted", 90);
        const silentPeerRun = await run("silentPeer", { OUTSIDE_PEER_SESSION: silentPeer });
        // dave's kept passkey and origin, beside a cookie that names no session, so `t5` goes
        // straight on to the passkey while the owner still lists him.
        const stillListed = join(scratch, "t5-still-listed");
        mkdirSync(stillListed, { mode: 0o700 });
        for (const name of ["origin", "passkey.json"]) {
          copyFileSync(join(scratch, "silentPeer", name), join(stillListed, name));
        }
        writeFileSync(join(stillListed, "cookie"), "dsession=no-such-session", { mode: 0o600 });
        const t5StillListed = await window("t5-still-listed", 90, {
          OUTSIDE_EDGE_STATE: stillListed,
        });
        const runs: StackRuns = {
          pass,
          passSession,
          ...held,
          tailnetCode,
          approve,
          approveCode,
          revoke,
          keptCredential,
          listedAfterRevoke,
          t5Listed,
          t5Delisted,
          t5StillListed,
          secondProject: await run("secondProject"),
          silentPeer: silentPeerRun,
          oneRequest: await run("oneRequest", { OUTSIDE_EDGE_LIMITS: oneRequestLimits }),
          signIn: await drive([], {
            OUTSIDE_EDGE_STEPS: "sign-in",
            OUTSIDE_EDGE_ALLOW_HTTP: "1",
            OUTSIDE_INVITE_URL: await invite(people.signIn),
            OUTSIDE_PROJECT: "OWN",
            OUTSIDE_EDGE_LIMITS: limitsFile,
            OUTSIDE_EDGE_STATE: join(scratch, "signIn"),
          }),
          outsideHost,
          edgeHost,
          registrationHosts: edge.hosts,
          edgeRefusals: edge.refusals,
        };
        console.log(JSON.stringify(runs));
      } finally {
        await browser.close();
        edge.stop();
      }
    } finally {
      await stopStacks(false);
    }
  } finally {
    rmSync(scratch, { force: true, recursive: true });
  }
}
