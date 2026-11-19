import { expect, test } from "bun:test";
import { fileURLToPath } from "node:url";
import { URL_NOT_PRINTED } from "@legion/contracts/log-text";
import { unshownSettingUrl } from "@legion/contracts/setting-url";
import {
  REDIRECT_CODES,
  type RedirectServers,
  redirectCase,
  redirectServers,
  UNNAMED_TARGETS,
} from "@legion/test-fixtures/redirect-servers";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { rowLimit, runBounded } from "./bounded-subprocess";

// The e2e client's `request` (e2e/api.ts) as a row or a script calls it: in a `bun` process of its
// own, since the module reads its server, `PLAYWRIGHT_BASE_URL` here, and its bearer when it loads.
// Each call carries a credential: the agent bearer and a handoff whose body holds a made-up key, or
// the session cookie of a dev sign-in and a comment, the sign-in answered by the first server
// itself.
const api = fileURLToPath(new URL("../api.ts", import.meta.url));
const scratch = temporaryDirectory("e2e-api-redirects");

/** The two calls of the client, each with the words its refusal names its credential by. */
const CALLS = [
  {
    carries: "the agent bearer",
    route: "/api/v1/issues/ACME-1/handoffs/implement",
    call: 'createHandoff("ACME-1", "implement", { key: "made-up-handoff-key" })',
  },
  {
    carries: "the session cookie",
    route: "/api/v1/issues/ACME-1/comments",
    call: 'createComment("ACME-1", { body: "A made-up comment." }, { login: "alice" })',
  },
] as const;

/** The sign-in call of the client: a comment as alice, whose session cookie it mints first. */
const [, SIGNED_IN] = CALLS;

/** The two servers: the first answers the dev sign-in itself and redirects every API call. */
function dispatchRedirects(): RedirectServers {
  return redirectServers({
    redirects: (pathname) => pathname.startsWith("/api/"),
    direct: () =>
      new Response(null, {
        status: 302,
        headers: { Location: "/", "Set-Cookie": "dsession=2.made-up.0.0.made-up-mac; Path=/" },
      }),
    atElsewhere: () => Response.json({ id: "h-elsewhere", version: 1 }, { status: 201 }),
  });
}

/** The two servers, the first answering the dev sign-in, and every other request, with what
 * `signIn` makes of the other server's origin. */
function signInAnswers(signIn: (elsewhere: string) => Response): RedirectServers {
  let elsewhere = "";
  const servers = redirectServers({ redirects: () => false, direct: () => signIn(elsewhere) });
  elsewhere = servers.elsewhere;
  return servers;
}

/** The message of the error that `call` of the client, against the first of `servers`, ended its
 * `bun` process with, as Bun prints it (the whole stderr when it printed none), and the exit. The
 * process is a bounded child under the short bound. */
async function run(
  servers: RedirectServers,
  call: string
): Promise<{ exit: number; died: string }> {
  const { code, stderr } = await runBounded({
    argv: ["bun", "-e", `import * as api from ${JSON.stringify(api)};\nawait api.${call};`],
    bound: "short",
    cwd: scratch,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: scratch,
      PLAYWRIGHT_BASE_URL: servers.origin,
      E2E_AGENT_TOKEN: "made-up-agent-token",
    },
    from: import.meta.url,
  });
  const line = stderr.split("\n").find((one) => one.startsWith("error: "));
  return { exit: code, died: line === undefined ? stderr : line.slice("error: ".length) };
}

/** What a refusal says to correct: for another path of the same address, that the client's path
 * is its own; for any other target, the setting that names Dispatch. */
function fix(sameAddress: boolean): string {
  return sameAddress
    ? "the client builds that path itself, so the server at that address, or a proxy in front of it, chose the other one: check what answers there"
    : "PLAYWRIGHT_BASE_URL, or DISPATCH_E2E_PORT where it is unset, must name Dispatch's own address";
}

/** The line `request` refuses a redirect of `route` with. */
function refusal(
  route: string,
  status: number,
  where: string,
  carries: string,
  sameAddress = false
): string {
  return `POST ${route} failed: ${status}, ${where}, and the e2e client follows no redirect, since it carries ${carries}: ${fix(sameAddress)}`;
}

/** The line the dev sign-in as alice refuses a redirect with. */
function signInRefusal(status: number, where: string, sameAddress = false): string {
  return `dev sign-in as alice failed: ${status}, ${where}, with no session cookie, and the e2e client follows no redirect: ${fix(sameAddress)}`;
}

// fetch would send a 307's or 308's body on to the origin a redirect names, a key in a handoff
// included, and take that origin's answer as Dispatch's (Bun and Node drop the bearer and the
// cookie alone). So no call follows a redirect: the 3xx is the answer, and the call fails naming
// it. One case for each code, so a client that follows again fails at each.
for (const code of REDIRECT_CODES) {
  test(
    redirectCase(code),
    async () => {
      const servers = dispatchRedirects();
      servers.code = code;
      try {
        for (const { carries, route, call } of CALLS) {
          const outcome = await run(servers, call);
          expect(servers.received, `the other origin after a ${code} of ${route}`).toEqual([]);
          expect(outcome).toEqual({
            exit: 1,
            died: refusal(route, code, `redirecting to ${servers.elsewhere}`, carries),
          });
        }
      } finally {
        servers.stop();
      }
    },
    // The two client processes of CALLS, one after the other.
    rowLimit("short", "short")
  );
}

// A call that read a redirect's body before its status would wait on a body that never ends, and
// refuse nothing.
test(
  "a redirect whose body never ends is refused naming its status, before any of its body is read",
  async () => {
    const servers = dispatchRedirects();
    servers.endlessBody = true;
    try {
      const [{ carries, route, call }] = CALLS;
      expect(await run(servers, call)).toEqual({
        exit: 1,
        died: refusal(route, 307, `redirecting to ${servers.elsewhere}`, carries),
      });
    } finally {
      servers.stop();
    }
  },
  rowLimit("short")
);

// A redirect within Dispatch's own address names no address the client lacks, and the client
// builds each route's path itself, whatever path the URL holds: its refusal says what to check.
// One that names nowhere is not named as Dispatch's own address, and a host longer than any host
// name is never printed.
test(
  "a redirect to another path of the same address, to no target, or to an overlong host is refused saying so",
  async () => {
    const servers = dispatchRedirects();
    try {
      const [{ carries, route, call }] = CALLS;
      const said: string[] = [];
      for (const { location } of UNNAMED_TARGETS) {
        servers.location = location;
        said.push((await run(servers, call)).died);
      }
      expect(said).toEqual(
        UNNAMED_TARGETS.map(({ where, sameAddress }) =>
          refusal(route, 307, where, carries, sameAddress)
        )
      );
    } finally {
      servers.stop();
    }
  },
  // One client process for each unnamed target, one after the other.
  rowLimit(...UNNAMED_TARGETS.map(() => "short" as const))
);

// The dev sign-in answers 302 with the session cookie. Any other 3xx, or a 302 with no cookie, is
// refused from its status, naming where it points, before any of its body is read: a body the
// answer chose is never printed, and one that never ends holds nothing up. One case for each code.
for (const code of REDIRECT_CODES) {
  test(
    `a dev sign-in redirected with no session cookie is refused naming its status, and none of its body is printed (${code})`,
    async () => {
      const servers = signInAnswers(
        (elsewhere) =>
          new Response("made-up-signin-body", {
            status: code,
            headers: { Location: `${elsewhere}/` },
          })
      );
      try {
        expect(await run(servers, SIGNED_IN.call)).toEqual({
          exit: 1,
          died: signInRefusal(code, `redirecting to ${servers.elsewhere}`),
        });
        expect(servers.received, `the other origin after a ${code}`).toEqual([]);
      } finally {
        servers.stop();
      }
    },
    rowLimit("short")
  );
}

test(
  "a dev sign-in redirected within its own address, with a body that never ends, is refused saying what to check",
  async () => {
    const servers = signInAnswers(
      () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(new TextEncoder().encode("made-up-signin-body that "));
            },
          }),
          { status: 302, headers: { Location: "/" } }
        )
    );
    try {
      expect(await run(servers, SIGNED_IN.call)).toEqual({
        exit: 1,
        died: signInRefusal(302, "redirecting to another path of the same address", true),
      });
    } finally {
      servers.stop();
    }
  },
  rowLimit("short")
);

/** The forms of a PLAYWRIGHT_BASE_URL with a made-up password that the URL parser does not read as
 *  an http or https URL with a host, one it does that carries a user part, and one with neither,
 *  which loads. */
function baseUrlForms(password: string) {
  return {
    unparsed: [
      `http://made-up-user:${password}@bad host:1`,
      `https://made-up-user:${password}/x@dispatch.example`,
      `made-up-user:${password}@dispatch.example`,
      `mailto:made-up-user:${password}@dispatch.example`,
      `//made-up-user:${password}@dispatch.example`,
      "   ",
    ],
    withUser: `http://made-up-user:${password}@127.0.0.1:1`,
    loads: "http://127.0.0.1:1",
  };
}

// The client reads PLAYWRIGHT_BASE_URL when it loads, and it may carry a user and a password: an
// honest one holding a `/` is enough for the URL parser to refuse the value, whose error would
// print it whole. A value that is no http or https URL with a host stops the load naming the
// setting and its length alone. One that carries a user part stops it too, named by its fixed
// phrase (`urlForText`): the fetch of Playwright's Node refuses such a URL with an error that
// prints it whole, and the client sends a credential of its own. A value with neither loads and
// prints nothing. The password is made for this run.
test(
  "a PLAYWRIGHT_BASE_URL that is no http or https URL with a host, or that carries a user part, stops the client's load printing no part of the password",
  async () => {
    const password = crypto.randomUUID();
    const { unparsed, withUser, loads } = baseUrlForms(password);
    const said: { exit: number; died: string; holds: boolean }[] = [];
    for (const base of [...unparsed, withUser, loads]) {
      const { code, stdout, stderr } = await runBounded({
        argv: ["bun", "-e", `import * as api from ${JSON.stringify(api)};\nvoid api;`],
        bound: "short",
        cwd: scratch,
        env: { PATH: process.env.PATH ?? "", HOME: scratch, PLAYWRIGHT_BASE_URL: base },
        from: import.meta.url,
      });
      const line = stderr.split("\n").find((one) => one.startsWith("error: "));
      said.push({
        exit: code,
        died: line === undefined ? stderr : line.slice("error: ".length),
        holds: `${stdout}${stderr}`.includes(password),
      });
    }
    // Each refusal names its URL in the one function's words (setting-url.test.ts holds a row per
    // cause of them).
    expect(said).toEqual([
      ...unparsed.map((base) => ({
        exit: 1,
        died: `PLAYWRIGHT_BASE_URL must be an http or https URL: ${unshownSettingUrl(base)}`,
        holds: false,
      })),
      {
        exit: 1,
        died: `PLAYWRIGHT_BASE_URL ${URL_NOT_PRINTED} must carry no user and no password: the fetch of Playwright's Node refuses a URL that carries them, printing it whole, and the e2e client sends a credential of its own`,
        holds: false,
      },
      { exit: 0, died: "", holds: false },
    ]);
  },
  // One client process for each form, one after the other.
  rowLimit(...baseUrlForms("").unparsed.map(() => "short" as const), "short", "short")
);
