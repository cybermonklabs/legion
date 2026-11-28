/** Which links are references is the origin list the server's /auth/whoami names for the address
 *  the request came to, by its Host, and the row reads it under two servers started with two
 *  lists, where a recording would answer one list for both. */

import { randomBytes, randomUUID } from "node:crypto";
import { createWriteStream, existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Readable } from "node:stream";
import { text } from "node:stream/consumers";
import { fileURLToPath } from "node:url";
import { expect, type Locator, type Page, test } from "@playwright/test";

import { createIssue, createProject } from "./api";
import { connectedDot, documentEditor } from "./editor";
import { harnessPorts, stackValue, withoutHarnessPort } from "./harness-ports";
import { oidcSignIn } from "./harness-signin";
import { Supervisor } from "./local-instance-support";
import { dropDatabase, sqlAt } from "./psql";

/**
 * A link at an address a Dispatch server answers at, or answered at before, is a reference in the
 * web page, and opens at the page's own origin (dispatch://INFRA-5491, c-l-refs). The page knows
 * those addresses only from the server's `GET /auth/whoami` answer, read at run time: the same web
 * build reads a link at a former address as a reference under a server that lists that address,
 * and as an ordinary link under one that does not.
 *
 * Each row stands on the real stack because what decides it is the server: the origins its
 * `/auth/whoami` names for the address the request came to, picked by the request's `Host`. So the
 * rows start servers of their own (`e2e/run-server.sh`, with `DISPATCH_SERVED_ORIGINS` and
 * `DISPATCH_FORMER_ORIGINS`) at the canonical address https://a.example.test, behind a TLS front
 * door of this file's own (`e2e/served-hosts-proxy.ts`) that keeps each request's `Host`. Chromium
 * maps every `*.example.test` host to that front door (`--host-resolver-rules`), so a page opened
 * at https://b.example.test is at that address for the browser and for the server alike, and its
 * context ignores the front door's throwaway certificate. A person signs in there through the dev
 * OIDC issuer, as under `DISPATCH_E2E_SIGNIN=oidc` (`e2e/harness-signin.ts`), whose one callback
 * for Dispatch's client is the served address's.
 *
 * A server refuses to start beside a live server process of its database that signs cookies with
 * another key, so the rows' servers cannot share the stack's database with the stack's own server:
 * each row makes a database of its own on the stack's Postgres and drops it at its end. That is why
 * this file keeps Playwright's own `test` rather than the suite's (e2e/test.ts), whose reset empties
 * the stack's database, which these rows never read.
 *
 * The row carries no `@pr` tag, so it runs on the full lane (the merge queue's run and the
 * nightly) and not on the pull-request lane: that lane is at its budget of 150 real-stack rows,
 * which `e2e/__tests__/pull-request-lane.test.ts` holds it to.
 */

/** The canonical address of the rows' servers (`DISPATCH_SERVER_URL`). */
const canonical = "https://a.example.test";
/** The address they answer at beside it (`DISPATCH_SERVED_ORIGINS`), where the page is opened. */
const served = "https://b.example.test";
/** The address the first server answered at before (`DISPATCH_FORMER_ORIGINS`). */
const former = "https://old.example.test";
/** An address that is none of a server's. */
const other = "https://other.example.test";

/** The person the issuer signs in at every authorization (`--sign-in-as`), a member of the
 *  sign-in pool group the servers admit. */
const person = oidcSignIn.person;

/** The words that open each paragraph of the document the row writes, one paragraph per address. */
const labels: Record<string, string> = {
  [canonical]: "At the canonical address",
  [served]: "At the served address",
  [former]: "At the former address",
  [other]: "At another host",
};

const dispatchPackage = fileURLToPath(new URL("..", import.meta.url));
const heldPortScript = fileURLToPath(
  new URL("../../../scripts/e2e/lib/held-port.py", import.meta.url)
);
const script = (name: string) => fileURLToPath(new URL(`./${name}`, import.meta.url));

/** How many of a process's last lines a refusal quotes. */
const tailLines = 20;

/** What every row of this file stands on, started once per worker: Chromium's `launchOptions`
 *  name the front door's port, and a browser is launched once per worker. */
interface ServedAddressesRig {
  /** The unix socket of the `held-port.py keep` that holds the port each row's servers serve on
   *  in turn (run-server.sh's `DISPATCH_E2E_LISTEN_FROM`), so the front door forwards to one port
   *  whichever server is up. */
  readonly keeper: string;
  /** That port, which the rows' API calls reach directly, past the front door. */
  readonly serverPort: number;
  /** The front door's port, which Chromium maps every example host to. */
  readonly proxyPort: number;
  /** The dev OIDC issuer's origin, and the secret of Dispatch's client there. */
  readonly issuer: string;
  readonly clientSecret: string;
}

/** Starts `argv` under `supervisor` in `env`, from this package's directory, and answers the first
 *  line of its output that `report` matches. Both of its streams are read to their end, so a full
 *  pipe never stops it, and copied to `log` where one is named. A process whose output ends before
 *  such a line is refused, naming how it exited and its last lines. */
function reported(
  supervisor: Supervisor,
  name: string,
  argv: readonly string[],
  env: NodeJS.ProcessEnv,
  report: RegExp,
  log?: string
): Promise<RegExpExecArray> {
  const running = supervisor.startPiped(name, argv, dispatchPackage, env);
  const file = log === undefined ? undefined : createWriteStream(log);
  const tail: string[] = [];
  const found = Promise.withResolvers<RegExpExecArray>();
  let match: RegExpExecArray | null = null;
  const see = (line: string) => {
    tail.push(line);
    if (tail.length > tailLines) tail.shift();
    if (match !== null) return;
    match = report.exec(line);
    if (match !== null) found.resolve(match);
  };
  const read = async (stream: Readable) => {
    const decoder = new TextDecoder();
    let partial = "";
    for await (const chunk of stream as AsyncIterable<Uint8Array>) {
      file?.write(chunk);
      const lines = (partial + decoder.decode(chunk, { stream: true })).split("\n");
      partial = lines.pop() ?? "";
      for (const line of lines) see(line);
    }
    partial += decoder.decode();
    if (partial !== "") see(partial);
  };
  void Promise.allSettled([read(running.child.stdout), read(running.child.stderr)]).then(
    async () => {
      file?.end();
      if (match !== null) return;
      const code = await running.exited.catch((error: unknown) => String(error));
      found.reject(
        new Error(
          `${name} exited ${code ?? running.child.signalCode} before it reported:\n${tail.join("\n")}`
        )
      );
    }
  );
  return found.promise;
}

const servedTest = test.extend<Record<never, never>, { rig: ServedAddressesRig }>({
  rig: [
    // Playwright refuses a first argument that is not a destructuring pattern.
    // biome-ignore lint/correctness/noEmptyPattern: the rig takes no other fixture.
    async ({}, use) => {
      const supervisor = new Supervisor();
      const dir = mkdtempSync(join(tmpdir(), "served-addresses-"));
      try {
        const portFile = join(dir, "server.port");
        const keeper = join(dir, "server.sock");
        const kept = supervisor.startPiped(
          "the port keeper",
          ["python3", heldPortScript, "keep", "--port-file", portFile, "--socket", keeper],
          dispatchPackage,
          process.env
        );
        const keeperError = text(kept.child.stderr).catch(() => "");
        void text(kept.child.stdout).catch(() => "");
        // The keeper writes the file through a rename once it serves its socket, so a file that
        // exists holds the whole port.
        await expect
          .poll(() => existsSync(portFile) || kept.child.exitCode !== null, { timeout: 30_000 })
          .toBe(true);
        if (!existsSync(portFile)) {
          throw new Error(
            `the port keeper exited ${kept.child.exitCode}: ${(await keeperError).trim()}`
          );
        }
        const serverPort = Number(readFileSync(portFile, "utf8").trim());
        const hosts = [canonical, served, former, other].map((origin) => new URL(origin).hostname);
        const [, proxyPort] = await reported(
          supervisor,
          "the served-hosts proxy",
          ["bun", script("served-hosts-proxy.ts"), String(serverPort), ...hosts],
          process.env,
          /^served-hosts proxy listening port=(\d+) /
        );
        // The issuer takes one callback for Dispatch's client, exactly: the served address's,
        // where the rows sign in. run-signin-issuer.sh builds it where DISPATCH_E2E_ISSUER_BIN names
        // no build, and a cold build is what the fixture's ten-minute timeout below is for.
        const clientSecret = randomBytes(32).toString("base64url");
        const [, issuer] = await reported(
          supervisor,
          "the dev OIDC issuer",
          ["bash", script("run-signin-issuer.sh")],
          {
            ...withoutHarnessPort("signinIssuer"),
            DISPATCH_E2E_SIGNIN: "oidc",
            DISPATCH_E2E_SIGNIN_AS: person,
            DISPATCH_E2E_SIGNIN_CALLBACK: `${served}/auth/callback`,
            DISPATCH_E2E_SIGNIN_CLIENT_ID: oidcSignIn.clientId,
            DISPATCH_E2E_SIGNIN_CLIENT_SECRET: clientSecret,
            DISPATCH_E2E_SIGNIN_GROUP: oidcSignIn.group,
          },
          /issuer listening url=(http:\/\/127\.0\.0\.1:\d+)$/
        );
        await use({ clientSecret, issuer, keeper, proxyPort: Number(proxyPort), serverPort });
      } finally {
        await supervisor.stop();
        rmSync(dir, { force: true, recursive: true });
      }
    },
    { scope: "worker", timeout: 600_000 },
  ],
  launchOptions: [
    async ({ rig }, use) => {
      await use({ args: [`--host-resolver-rules=MAP *.example.test 127.0.0.1:${rig.proxyPort}`] });
    },
    { scope: "worker" },
  ],
});

/** What run-server.sh reads of this worker's stack, under the plain names a pooled run carries in
 *  its manifest alone: its fakes' ports, and how many stacks share the machine. The database is
 *  `database`, the row's own, and the stack's own port is left out, since a row's server serves on
 *  the rig's. The rows' servers keep no file, embed nothing and verify no service account, so a
 *  run that gives its stack a file store, an embedder or an access issuer gives them none. */
function stackInputs(database: string): NodeJS.ProcessEnv {
  const env = withoutHarnessPort("dispatch");
  delete env.DISPATCH_E2E_FILE_STORE_ENDPOINT;
  delete env.DISPATCH_E2E_EMBEDDER;
  delete env.DISPATCH_E2E_ACCESS_ISSUER;
  const ports = harnessPorts();
  for (const [variable, port] of [
    ["FAKE_BROKER_PORT", ports.fakeBroker.port],
    ["FAKE_ENVOY_PORT", ports.fakeEnvoy.port],
    ["FAKE_GITHUB_PORT", ports.fakeGithub.port],
  ] as const) {
    if (port !== 0) env[variable] = String(port);
  }
  const stacks = stackValue(process.env, "DISPATCH_E2E_STACK_COUNT");
  if (stacks !== undefined) env.DISPATCH_E2E_STACK_COUNT = stacks;
  return { ...env, DATABASE_URL: database };
}

/** Starts a Dispatch server on `database` at the canonical address on the rig's port, signing
 *  people in through the rig's issuer with `signingKey`, serving `served` and naming
 *  `formerOrigins` as its former addresses (none when undefined), and returns the supervisor that
 *  stops it once it answers. Its output goes to `log`. */
async function startServer(
  rig: ServedAddressesRig,
  database: string,
  signingKey: string,
  formerOrigins: string | undefined,
  log: string
): Promise<Supervisor> {
  const supervisor = new Supervisor();
  const name = `the Dispatch server ${formerOrigins === undefined ? "with no former address" : `whose former address is ${formerOrigins}`}`;
  try {
    await reported(
      supervisor,
      name,
      ["bash", script("run-server.sh")],
      {
        ...stackInputs(database),
        DISPATCH_E2E_LISTEN_FROM: rig.keeper,
        DISPATCH_E2E_SERVER_URL: canonical,
        DISPATCH_E2E_SIGNIN: "oidc",
        DISPATCH_E2E_SIGNIN_CLIENT_ID: oidcSignIn.clientId,
        DISPATCH_E2E_SIGNIN_CLIENT_SECRET: rig.clientSecret,
        DISPATCH_E2E_SIGNIN_GROUP: oidcSignIn.group,
        DISPATCH_E2E_SIGNIN_ISSUER_PORT: new URL(rig.issuer).port,
        DISPATCH_E2E_SIGNING_KEY: signingKey,
        DISPATCH_SERVED_ORIGINS: served,
        ...(formerOrigins === undefined ? {} : { DISPATCH_FORMER_ORIGINS: formerOrigins }),
      },
      /msg="dispatch: listening" addr=127\.0\.0\.1:(\d+)(?:\s|$)/,
      log
    );
    await expect
      .poll(
        async () => {
          const answer = await fetch(`http://127.0.0.1:${rig.serverPort}/healthz`, {
            signal: AbortSignal.timeout(5_000),
          }).catch(() => undefined);
          return answer?.status;
        },
        { message: `${name} answers 200 at /healthz`, timeout: 30_000 }
      )
      .toBe(200);
  } catch (error) {
    await supervisor.stop();
    throw error;
  }
  return supervisor;
}

/** Signs `person` in at the served address in `page`: the page's "Sign in with Google", which goes
 *  through the issuer's `/authorize`, where the issuer signs `person` in, and back to the served
 *  address's `/auth/callback`. An authorization the issuer refuses fails the sign-in with the
 *  issuer's own answer, and a callback that answers anything but its redirect fails it with the
 *  callback's. */
async function signIn(page: Page, issuer: string): Promise<void> {
  await page.goto(`${served}/`);
  const answered = page.waitForResponse((response) => {
    const { origin, pathname } = new URL(response.url());
    return (
      (origin === served && pathname === "/auth/callback") ||
      (origin === issuer && response.status() >= 400)
    );
  });
  await page.getByRole("link", { name: "Sign in with Google" }).click({ noWaitAfter: true });
  const answer = await answered;
  if (new URL(answer.url()).origin === issuer) {
    throw new Error(
      `the issuer answered the sign-in at ${served} ${answer.status()}: ${(await answer.text()).trim()}`
    );
  }
  // A redirect's body cannot be read; any other answer's body says why the sign-in stopped.
  const why = answer.status() === 302 ? "" : ` ${await answer.text()}`;
  expect(answer.status(), `/auth/callback for ${person} answered ${answer.status()}${why}`).toBe(
    302
  );
}

/** The paragraph of `editor` that holds the link at `origin`. */
function paragraphOf(editor: Locator, origin: string): Locator {
  return editor.locator("p").filter({ hasText: `${labels[origin]}:` });
}

/** Opens the document at the served address in `page`, once its editor shows every paragraph and
 *  its live room is connected through the front door. */
async function openDocument(page: Page, issueKey: string): Promise<Locator> {
  await page.goto(`${served}/issues/${issueKey}/spec`);
  const editor = documentEditor(page);
  for (const label of Object.values(labels)) await expect(editor).toContainText(label);
  await expect(connectedDot(page)).toHaveText("connected");
  return editor;
}

/** `url`, a libpq URL of the stack's Postgres, naming the database `name` in place of its own. The
 *  harness's URL names its server by a socket directory in its query and no host
 *  (`postgres://<user>@/<database>?host=<dir>`, `socketUrl` in e2e/harness-postgres.ts), which
 *  WHATWG's `URL` refuses, so the path between the authority and the query is replaced as text. */
function databaseUrl(url: string, name: string): string {
  const parts = /^(postgres(?:ql)?:\/\/[^/?#]*)(?:\/[^?#]*)?(.*)$/.exec(url);
  if (parts === null) throw new Error("this stack's DATABASE_URL is no postgres:// URL");
  return `${parts[1]}/${name}${parts[2]}`;
}

// A deployed run has no stack Postgres for a row to make its database on and start its servers
// against.
servedTest.skip(
  Boolean(process.env.PLAYWRIGHT_BASE_URL),
  "it starts servers of its own on this stack's Postgres, which a deployed run has none of"
);

// SLOW(>=2s): it starts a Dispatch server twice, the first on an empty database it migrates, and
// signs a person in through the dev OIDC issuer.
servedTest(
  "a link at a served or former address opens at the page's own origin, and is an ordinary link under a server that lists no former address",
  async ({ browser, rig }, testInfo) => {
    const stackDatabase = stackValue(process.env, "DATABASE_URL");
    if (stackDatabase === undefined) throw new Error("this stack names no DATABASE_URL");
    const name = `served_addresses_${randomUUID().replaceAll("-", "_")}`;
    const database = databaseUrl(stackDatabase, name);
    const signingKey = randomBytes(32).toString("base64url");
    const servers: Supervisor[] = [];
    const context = await browser.newContext({ ignoreHTTPSErrors: true });
    try {
      await sqlAt(stackDatabase, [`create database ${name}`]);
      servers.push(
        await startServer(
          rig,
          database,
          signingKey,
          former,
          testInfo.outputPath("dispatch-with-former.log")
        )
      );
      const page = await context.newPage();
      await signIn(page, rig.issuer);

      // The document is written through the API, past the front door, as the agent session every
      // server of this harness takes; the project's default assignee is the person who signed in.
      const agent = { as: "agent", baseUrl: `http://127.0.0.1:${rig.serverPort}` } as const;
      await createProject(
        { default_assignee: person, key: "SRV", name: "Served addresses" },
        agent
      );
      const target = await createIssue(
        { force: true, project: "SRV", title: "The issue every link names" },
        agent
      );
      const path = `/issues/${target.key}`;
      const document = await createIssue(
        {
          force: true,
          project: "SRV",
          spec: Object.entries(labels)
            .map(([origin, label]) => `${label}: ${origin}${path}`)
            .join("\n\n"),
          title: "The document that links to it",
        },
        agent
      );

      let editor = await openDocument(page, document.key);
      for (const origin of [canonical, served, former]) {
        await expect(
          paragraphOf(editor, origin).locator("a.dispatch-reference"),
          `the link at ${origin} is a reference to ${target.key}`
        ).toHaveAttribute("data-dispatch-href", `dispatch://${target.key}`);
      }
      const elsewhere = paragraphOf(editor, other);
      await expect(elsewhere.getByRole("link", { name: `${other}${path}` })).toHaveAttribute(
        "href",
        `${other}${path}`
      );
      await expect(elsewhere.locator("a.dispatch-reference")).toHaveCount(0);

      await paragraphOf(editor, former).locator("a.dispatch-reference").click();
      await expect(page).toHaveURL(`${served}${path}`);
      await expect(page.getByRole("heading", { name: target.title }).first()).toBeVisible();

      // The same web build, served by a server whose former list is empty: one session signs in at
      // both, since they share the database and the cookie key.
      await page.close();
      await servers[0].stop();
      servers.push(
        await startServer(
          rig,
          database,
          signingKey,
          undefined,
          testInfo.outputPath("dispatch-without-former.log")
        )
      );
      const reopened = await context.newPage();
      editor = await openDocument(reopened, document.key);
      await expect(
        paragraphOf(editor, canonical).locator("a.dispatch-reference"),
        `the link at ${canonical} is still a reference to ${target.key}`
      ).toHaveAttribute("data-dispatch-href", `dispatch://${target.key}`);
      const old = paragraphOf(editor, former);
      await expect(old.getByRole("link", { name: `${former}${path}` })).toHaveAttribute(
        "href",
        `${former}${path}`
      );
      await expect(old.locator("a.dispatch-reference")).toHaveCount(0);
    } finally {
      await context.close();
      for (const server of servers) await server.stop();
      await dropDatabase(stackDatabase, name);
    }
  }
);
