/** An outside origin that answers as Dispatch's does in the few ways the outside driver's controls
 *  (`outside-edge.test.ts`) need, and in no other: each control starts one, and the driver must
 *  read what it answers as UNKNOWN or FAIL where a real refusal, sign-in or revocation is missing.
 *
 *  - `GET /auth/whoami` is 401, as for a cookie Dispatch no longer takes, so `t5` goes on to the
 *    passkey; to a cookie this stand-in set at a sign-in, it answers `whoami` (200 with the person
 *    where unset).
 *  - `GET /` is a page with a "Sign in with a passkey" button. Its click shows a `role="alert"`
 *    and nothing else: with `finish` absent, Dispatch's own alert for a request that reached
 *    nobody, "Dispatch could not be reached. Try again.", and no request at all, as the sign-in
 *    page shows when its first request gets no answer; with `finish` set, it posts
 *    `/auth/passkey/login/finish`, which answers `finish`'s status with
 *    `{"error", "code"}` (or a plain-text body where `code` is absent), and alerts the answer's
 *    `error`, or `answered <status>`; with `finish.stallBody`, that answer sends its status and
 *    the start of its body, and never the rest, so the page shows nothing after it.
 *  - `GET /enroll` is the enrollment page `enroll` names (`Enrollment`): by default one that
 *    refuses its link in an alert naming the whole link, its fragment, the token, included, what a
 *    page that echoes its URL would show; or one whose "Create a passkey" makes a passkey on the
 *    browser's authenticator, signs the person in (`POST /__sign-in`, which sets this stand-in's
 *    cookie) and says "Signed in as …", or that misses one part of that. With `blocks`, a page
 *    that signed the person in then holds its main thread (`Blocks`).
 *  - `GET /api/v1/projects` lists OWN, or answers `projects`'s status.
 *  - `POST /v1/oauth/device_authorization` and the device grant of `POST /v1/oauth/token` are a
 *    broker's device login on this origin (`broker`), approved once the machine-login page
 *    approved its code; with `poll`, the device grant's polls are never answered (`silent`),
 *    answered `authorization_pending` once and then never (`once`), or answered
 *    `authorization_pending` every time (`pending`). The token exchange of `POST /v1/oauth/token`
 *    mints a session's token for a subject token one of those logins carried, and refuses any
 *    other (`tradeRefusal`), so a run whose device login passed opens its relay socket at the
 *    listener it names. With `exchange`, the exchange is never answered (`silent`), or answered
 *    only after that many milliseconds.
 *  - `GET /credentials/machine` is the machine-login page `machine` names (`MachinePage`): by
 *    default one that looks a typed code up, approves it and names its credential; or one that
 *    misses one part of that.
 *  - `GET /api/v1/machine-logins` lists `listed` as `{"credentials": [{"credential_id"}]}`, and
 *    `POST /api/v1/machine-logins/{id}/revoke` answers 200 for a listed id, taking it off the list
 *    only where `revokeRemoves` holds; any other id is 404. With `silentAfterRevoke`, the list
 *    answers nothing once a revoke was answered.
 *  Every path asked is kept in `asked`, in order.
 */

import { tradeRefusal } from "./outside-edge-credentials";

/** The enrollment page's kinds: `refuses`, an alert naming the whole link; `signs-in`, Create a
 *  passkey makes one, the stand-in sets its cookie and the page says "Signed in as …"; and the
 *  ways that falls short: `no-button`, neither the button nor an alert; `never-loads`, a page
 *  whose load never ends; `no-sign-in`, a button that does nothing but tell the stand-in it was
 *  pressed (`POST /__clicked`, kept in `asked`); `refused`, a button whose
 *  click alerts a refusal; `no-passkey`, a sign-in with no passkey made; `elsewhere`, a sign-in
 *  that lands on another origin; `no-cookie`, a sign-in that sets no cookie. */
export type Enrollment =
  | "refuses"
  | "signs-in"
  | "no-button"
  | "never-loads"
  | "no-sign-in"
  | "refused"
  | "no-passkey"
  | "elsewhere"
  | "no-cookie";

/** The machine-login page's kinds: `approves`, it looks the typed code up, approves it and names
 *  its credential; and the ways that falls short: `no-box`, no code box; `untypable`, a code box
 *  that takes no typing; `no-answer`, a look-up that shows nothing; `refuses-code`, a look-up
 *  that alerts a refusal; `refuses-code-lines`, the same refusal on two lines, the second
 *  `APPROVED ABCD-EFGH`, as a page whose text holds a line break would show; `refuses-approval`,
 *  an approval that alerts a refusal; `never-approved`, an Approve that shows nothing;
 *  `no-credential`, an approval that names no credential. */
export type MachinePage =
  | "approves"
  | "no-box"
  | "untypable"
  | "no-answer"
  | "refuses-code"
  | "refuses-code-lines"
  | "refuses-approval"
  | "never-approved"
  | "no-credential";

/** The device login's broker on this origin: `answers`, its page this origin's and its poll
 *  answered the pair once the page approved the code; `refuses`, the device authorization
 *  refused; `elsewhere`, a page on another origin; `denies`, the poll answered `access_denied`. */
export type DeviceBroker = "answers" | "refuses" | "elsewhere" | "denies";

/** How a `signs-in` enrollment page holds its main thread once it has signed the person in, so
 *  that no script of the page's, nor one the driver runs in it, runs again: `read`, its `fetch`
 *  loops forever, so the driver's first read through the page (`GET /auth/whoami`) holds it;
 *  `after-reads`, it loops once its second read (`GET /api/v1/projects`) has handed back its
 *  answer, so the driver's save of the browser's state, which reads the page's storage through a
 *  script in it, finds it held. The second hands its answer back as the driver's read takes it
 *  (`status` and `text()`, its body already read), so the read ends before the loop starts. */
export type Blocks = "read" | "after-reads";

/** What one stand-in answers. */
export interface OriginBehaviour {
  /** The answer of `POST /auth/passkey/login/finish`; absent, the page never posts it. With
   *  `stallBody`, the answer's status and the start of its body are sent, and the rest never. */
  readonly finish?: {
    readonly status: number;
    readonly code?: string;
    readonly stallBody?: boolean;
  };
  /** The credential ids the person's machine-logins list holds at the start. */
  readonly listed?: readonly string[];
  /** Whether a revoke takes its id off the list, as the broker's revoke does. */
  readonly revokeRemoves?: boolean;
  /** Whether the list answers nothing at all once a revoke was answered. */
  readonly silentAfterRevoke?: boolean;
  /** The enrollment page; `refuses` where unset. */
  readonly enroll?: Enrollment;
  /** What `GET /auth/whoami` answers a cookie this stand-in set: its status, 200 with the person
   *  where unset; `silent`, no answer at all; or `stalled`, a 200 whose body starts and never
   *  ends (`stalledJson`). */
  readonly whoami?: number | "silent" | "stalled";
  /** The status `GET /api/v1/projects` answers; 200, listing OWN, where unset. */
  readonly projects?: number;
  /** The device login's broker; `answers` where unset. */
  readonly broker?: DeviceBroker;
  /** The device grant's polls: never answered, answered pending once and then never, or answered
   *  pending every time; answered as `broker` says once the code is approved, where unset. */
  readonly poll?: "silent" | "once" | "pending";
  /** The machine-login page; `approves` where unset. */
  readonly machine?: MachinePage;
  /** The token exchange: `silent`, never answered; a number, answered after that many
   *  milliseconds; answered at once where unset. */
  readonly exchange?: "silent" | number;
  /** How a `signs-in` enrollment page holds its main thread once it signed the person in. */
  readonly blocks?: Blocks;
}

export interface OriginStandIn {
  /** `http://127.0.0.1:<port>`. */
  readonly url: string;
  /** The same server as `http://localhost:<port>`, the origin a sign-in and a device login's
   *  page are on: a passkey's relying party is a host name, never an address. */
  readonly page: string;
  /** Another origin, where an `elsewhere` sign-in lands and an `elsewhere` broker's page is. */
  readonly elsewhere: string;
  /** Every id a revoke named, in order. */
  readonly revoked: readonly string[];
  /** Every path this origin was asked, in order. */
  readonly asked: readonly string[];
  stop(): void;
}

/** Dispatch's alert for a request that reached nobody (web/src/features/signin/refusals.ts). */
export const unreachedAlert = "Dispatch could not be reached. Try again.";

/** The person the stand-in signs in. */
export const standInPerson = "carol@outside.example";

/** The credential the stand-in's machine-login page names for an approved code. */
export const standInCredential = "cred-stand-in";

/** The page's own helper, which adds a line of `role` (or none) saying `text`. */
const sayScript = `function say(text, role) {
  const line = document.createElement(role === undefined ? "p" : "div");
  if (role !== undefined) line.setAttribute("role", role);
  line.textContent = text;
  document.querySelector("main").append(line);
}`;

/** The grant type of the broker's token exchange (RFC 8693), which trades a device login for a
 *  session's token. */
const TOKEN_EXCHANGE = "urn:ietf:params:oauth:grant-type:token-exchange";

/** An answer of `status` whose status and the start of its JSON body go out, so the browser has
 *  its headers, and whose body never ends. */
function stalledJson(status: number): Response {
  const start = new TextEncoder().encode('{"error":');
  return new Response(new ReadableStream({ start: (controller) => controller.enqueue(start) }), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function signInPage(finish: OriginBehaviour["finish"]): string {
  const click =
    finish === undefined
      ? `say(${JSON.stringify(unreachedAlert)}, "alert");`
      : `try {
          const answer = await fetch("/auth/passkey/login/finish", { method: "POST", body: "{}" });
          const body = await answer.json().catch(() => ({}));
          say(typeof body.error === "string" ? body.error : "answered " + answer.status, "alert");
        } catch {
          say(${JSON.stringify(unreachedAlert)}, "alert");
        }`;
  return `<!doctype html><html><body><main>
<button type="button" id="sign-in">Sign in with a passkey</button>
<script>
${sayScript}
document.getElementById("sign-in").onclick = async () => {
  ${click}
};
</script></main></body></html>`;
}

/** The enrollment page of `kind`; `elsewhere` is the origin an `elsewhere` sign-in lands on, and
 *  `blocks` how a page that signed the person in then holds its main thread. */
function enrollPage(kind: Enrollment, elsewhere: string, blocks: Blocks | undefined): string {
  if (kind === "refuses") {
    return `<!doctype html><html><body><main>
<script>
${sayScript}
say("This link is not one Dispatch made: " + location.href, "alert");
</script></main></body></html>`;
  }
  if (kind === "no-button") {
    return "<!doctype html><html><body><main><p>Welcome.</p></main></body></html>";
  }
  if (kind === "never-loads") {
    // A script the stand-in never answers: the page's load event never comes.
    return `<!doctype html><html><body><main><p>Welcome.</p><script src="/never"></script></main></body></html>`;
  }
  const click =
    kind === "no-sign-in"
      ? `fetch("/__clicked", { method: "POST" });`
      : kind === "refused"
        ? `say("This invitation was withdrawn.", "alert");`
        : `try {
    if (${JSON.stringify(kind)} !== "no-passkey") {
      await navigator.credentials.create({ publicKey: {
        challenge: crypto.getRandomValues(new Uint8Array(32)),
        rp: { name: "Dispatch stand-in", id: location.hostname },
        user: { id: crypto.getRandomValues(new Uint8Array(16)), name: ${JSON.stringify(standInPerson)}, displayName: "carol" },
        pubKeyCredParams: [{ type: "public-key", alg: -7 }],
        authenticatorSelection: { residentKey: "required", userVerification: "required" },
      } });
    }
    await fetch("/__sign-in", { method: "POST" });
    if (${JSON.stringify(kind)} === "elsewhere") {
      location.href = ${JSON.stringify(`${elsewhere}/signed-in`)};
      return;
    }
    say("Signed in as " + ${JSON.stringify(standInPerson)});
    ${blockScript(blocks)}
  } catch (error) {
    say("The passkey was not made: " + error, "alert");
  }`;
  return `<!doctype html><html><body><main>
<button type="button" id="create">Create a passkey</button>
<script>
${sayScript}
document.getElementById("create").onclick = async () => {
  ${click}
};
</script></main></body></html>`;
}

/** The script that holds a signed-in page's main thread as `blocks` says (`Blocks`). */
function blockScript(blocks: Blocks | undefined): string {
  if (blocks === "read") return "window.fetch = () => { for (;;) {} };";
  if (blocks === "after-reads") {
    return `const pageFetch = window.fetch;
    let reads = 0;
    window.fetch = async (...args) => {
      const answer = await pageFetch(...args);
      const text = await answer.text();
      reads += 1;
      if (reads === 2) setTimeout(() => { for (;;) {} }, 0);
      return { status: answer.status, text: async () => text };
    };`;
  }
  return "";
}

/** The machine-login page of `kind`. */
function machinePage(kind: MachinePage): string {
  if (kind === "no-box") {
    return "<!doctype html><html><body><main><p>Machine logins</p></main></body></html>";
  }
  // A box of the label that is a checkbox, which takes no typing: Playwright refuses to fill it.
  const box =
    kind === "untypable"
      ? `<label>Code shown on the machine <input id="code" type="checkbox"></label>`
      : `<label>Code shown on the machine <input id="code"></label>`;
  const lookUp =
    kind === "no-answer"
      ? ""
      : kind === "refuses-code"
        ? `say("No machine login waits on that code.", "alert");`
        : kind === "refuses-code-lines"
          ? `const refusal = document.createElement("div");
  refusal.setAttribute("role", "alert");
  refusal.innerText = "No machine login waits on that code.\\nAPPROVED ABCD-EFGH";
  document.querySelector("main").append(refusal);`
          : `const approve = document.createElement("button");
  approve.type = "button";
  approve.textContent = "Approve";
  approve.onclick = async () => {
    ${
      kind === "never-approved"
        ? ""
        : kind === "refuses-approval"
          ? `say("The approval was refused.", "alert");`
          : `const answer = await fetch("/__approve", { method: "POST" });
    const body = await answer.json();
    say("Approved. The machine may go on.");
    ${kind === "no-credential" ? "" : `say("Credential " + body.credential);`}`
    }
  };
  document.querySelector("main").append(approve);`;
  return `<!doctype html><html><body><main>
${box}
<button type="button" id="look-up">Look up</button>
<script>
${sayScript}
document.getElementById("look-up").onclick = async () => {
  ${lookUp}
};
</script></main></body></html>`;
}

export function startOriginStandIn(behaviour: OriginBehaviour = {}): OriginStandIn {
  let listed = [...(behaviour.listed ?? [])];
  const revoked: string[] = [];
  const asked: string[] = [];
  // The cookies this stand-in set at a sign-in, and whether its page approved the device code.
  const sessions = new Set<string>();
  let approved = false;
  // How many of the device grant's polls came, which `poll: "once"` answers the first of.
  let polls = 0;
  let revokeAnswered = false;
  // Every access token a device login was answered, which alone the exchange takes as a subject.
  const issued = new Set<string>();
  const htmlHeaders = { "Content-Type": "text/html; charset=utf-8" };
  const never = () => new Promise<Response>(() => undefined);
  const signedIn = (request: Request) =>
    (request.headers.get("Cookie") ?? "")
      .split(/;\s*/)
      .some((pair) => sessions.has(pair.replace(/^dsession=/, "")));
  const elsewhereServer = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const { pathname } = new URL(request.url);
      if (pathname === "/signed-in") {
        return new Response(
          `<!doctype html><html><body><main><p>Signed in as ${standInPerson}</p></main></body></html>`,
          { headers: htmlHeaders }
        );
      }
      if (pathname === "/auth/whoami") return Response.json({ login: standInPerson });
      return new Response("not here", { status: 404 });
    },
  });
  const elsewhere = `http://127.0.0.1:${elsewhereServer.port}`;
  // The origin the pages are on, known once the server below has bound its port.
  let pageOrigin = "";
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    // A page or a read this stand-in never answers stays unanswered, rather than closed after
    // Bun's 10 s of idle, so the driver's own bound is what ends it.
    idleTimeout: 0,
    async fetch(request): Promise<Response> {
      const url = new URL(request.url);
      const { pathname } = url;
      asked.push(pathname);
      if (pathname === "/auth/whoami") {
        // A `no-cookie` sign-in names its person with no cookie at all, so the driver's own
        // cookie check is what refuses it.
        if (!signedIn(request) && behaviour.enroll !== "no-cookie") {
          return Response.json({ error: "no session" }, { status: 401 });
        }
        if (behaviour.whoami === "silent") return never();
        if (behaviour.whoami === "stalled") return stalledJson(200);
        const status = behaviour.whoami ?? 200;
        return status === 200
          ? Response.json({ login: standInPerson })
          : Response.json({ error: "the stand-in refuses this read" }, { status });
      }
      if (request.method === "GET" && pathname === "/") {
        return new Response(signInPage(behaviour.finish), { headers: htmlHeaders });
      }
      if (request.method === "GET" && pathname === "/enroll") {
        return new Response(
          enrollPage(behaviour.enroll ?? "refuses", elsewhere, behaviour.blocks),
          { headers: htmlHeaders }
        );
      }
      if (pathname === "/never") return never();
      if (request.method === "POST" && pathname === "/__sign-in") {
        if (behaviour.enroll === "no-cookie") return Response.json({ signedIn: true });
        const session = `stand-in-${crypto.randomUUID()}`;
        sessions.add(session);
        return Response.json(
          { signedIn: true },
          { headers: { "Set-Cookie": `dsession=${session}; Path=/; HttpOnly; SameSite=Lax` } }
        );
      }
      if (request.method === "GET" && pathname === "/api/v1/projects") {
        const status = behaviour.projects ?? 200;
        return status === 200
          ? Response.json([{ key: "OWN" }])
          : Response.json({ error: "the stand-in refuses this list" }, { status });
      }
      if (request.method === "POST" && pathname === "/auth/passkey/login/finish") {
        const finish = behaviour.finish;
        if (finish === undefined) return new Response("not here", { status: 404 });
        if (finish.code === undefined) return new Response("refused", { status: finish.status });
        if (finish.stallBody === true) return stalledJson(finish.status);
        return Response.json(
          { error: `the stand-in answers ${finish.code}`, code: finish.code },
          { status: finish.status }
        );
      }
      if (request.method === "POST" && pathname === "/v1/oauth/device_authorization") {
        if (behaviour.broker === "refuses") {
          return Response.json(
            { error: "access_denied", error_description: "the stand-in refuses this device login" },
            { status: 400 }
          );
        }
        const page = behaviour.broker === "elsewhere" ? elsewhere : pageOrigin;
        return Response.json({
          device_code: `stand-in-device-${crypto.randomUUID()}`,
          user_code: "STND-CODE",
          verification_uri: `${page}/credentials/machine`,
          expires_in: 600,
          interval: 1,
        });
      }
      if (request.method === "POST" && pathname === "/v1/oauth/token") {
        const form = new URLSearchParams(await request.text());
        if (form.get("grant_type") === TOKEN_EXCHANGE) {
          if (behaviour.exchange === "silent") return never();
          if (behaviour.exchange !== undefined) await Bun.sleep(behaviour.exchange);
          return (
            tradeRefusal(form, issued) ??
            Response.json({
              access_token: `stand-in-session-${crypto.randomUUID()}`,
              issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
              token_type: "DPoP",
              expires_in: 900,
            })
          );
        }
        polls += 1;
        if (behaviour.poll === "silent" || (behaviour.poll === "once" && polls > 1)) return never();
        if (!approved || behaviour.poll !== undefined) {
          return Response.json({ error: "authorization_pending" }, { status: 400 });
        }
        if (behaviour.broker === "denies") {
          return Response.json({ error: "access_denied" }, { status: 400 });
        }
        const accessToken = `stand-in-login-${crypto.randomUUID()}`;
        issued.add(accessToken);
        return Response.json({
          access_token: accessToken,
          token_type: "DPoP",
          expires_in: 900,
          refresh_token: `stand-in-refresh-${crypto.randomUUID()}`,
          scope: "machine",
          credential_id: standInCredential,
          credential_expires_at: new Date(Date.now() + 86_400_000)
            .toISOString()
            .replace(/\.\d{3}Z$/, "Z"),
        });
      }
      if (request.method === "GET" && pathname === "/credentials/machine") {
        return new Response(machinePage(behaviour.machine ?? "approves"), {
          headers: htmlHeaders,
        });
      }
      if (request.method === "POST" && pathname === "/__approve") {
        approved = true;
        return Response.json({ credential: standInCredential });
      }
      if (request.method === "GET" && pathname === "/api/v1/machine-logins") {
        if (behaviour.silentAfterRevoke === true && revokeAnswered) return never();
        return Response.json({ credentials: listed.map((id) => ({ credential_id: id })) });
      }
      const revoke = /^\/api\/v1\/machine-logins\/([^/]+)\/revoke$/.exec(pathname);
      if (request.method === "POST" && revoke !== null) {
        const id = decodeURIComponent(revoke[1] ?? "");
        revoked.push(id);
        if (!listed.includes(id)) return Response.json({ code: "NOT_FOUND" }, { status: 404 });
        if (behaviour.revokeRemoves === true) listed = listed.filter((one) => one !== id);
        revokeAnswered = true;
        return Response.json({ state: "revoked" });
      }
      return new Response("not here", { status: 404 });
    },
  });
  pageOrigin = `http://localhost:${server.port}`;
  return {
    url: `http://127.0.0.1:${server.port}`,
    page: pageOrigin,
    elsewhere,
    revoked,
    asked,
    stop: () => {
      server.stop(true);
      elsewhereServer.stop(true);
    },
  };
}
