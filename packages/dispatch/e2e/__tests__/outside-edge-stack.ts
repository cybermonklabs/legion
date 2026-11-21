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
 *    `approve` of a third code of hers, `revoke`, and `t5` twice: while she is listed, and after
 *    the owner took her off OWN.
 *  - `secondProject`: bob, on OWN and OTHER: the run must fail step 1 naming OTHER.
 *  - `silentPeer`: dave, on OWN alone, sending to a peer that never answers: step 3 must fail by
 *    timeout, naming that peer. Then `t5` with his kept passkey and a cookie Dispatch refuses,
 *    while he is still listed: the passkey must sign him in, the positive control of carol's
 *    refused one.
 *  - `oneRequest`: erin, on OWN alone, with the listener's threshold of a minute set to 1: step 4
 *    must fail, naming the host.
 */

import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
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
  /** The edge's two hosts of this stack, as the limits file names them. */
  readonly outsideHost: string;
  readonly edgeHost: string;
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
        const pass = await run("pass", { OUTSIDE_TAILNET_USER_CODE: tailnetCode });
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
          outsideHost,
          edgeHost,
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
