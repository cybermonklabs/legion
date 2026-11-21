/** An outside origin that answers as Dispatch's does in the few ways the outside driver's controls
 *  (`outside-edge.test.ts`) need, and in no other: each control starts one, and the driver must
 *  read what it answers as UNKNOWN or FAIL where a real refusal or revocation is missing.
 *
 *  - `GET /auth/whoami` is 401, as for a cookie Dispatch no longer takes, so `t5` goes on to the
 *    passkey.
 *  - `GET /` is a page with a "Sign in with a passkey" button. Its click shows a `role="alert"`
 *    and nothing else: with `finish` absent, Dispatch's own alert for a request that reached
 *    nobody, "Dispatch could not be reached. Try again.", and no request at all, as the sign-in
 *    page shows when its first request gets no answer; with `finish` set, it posts
 *    `/auth/passkey/login/finish`, which answers `finish`'s status with
 *    `{"error", "code"}` (or a plain-text body where `code` is absent), and alerts the answer's
 *    `error`, or `answered <status>`.
 *  - `GET /enroll` is a page that refuses its link in an alert naming the whole link, its
 *    fragment, the token, included: what a page that echoes its URL would show.
 *  - `GET /api/v1/machine-logins` lists `listed` as `{"credentials": [{"credential_id"}]}`, and
 *    `POST /api/v1/machine-logins/{id}/revoke` answers 200 for a listed id, taking it off the list
 *    only where `revokeRemoves` holds; any other id is 404.
 */

/** What one stand-in answers. */
export interface OriginBehaviour {
  /** The answer of `POST /auth/passkey/login/finish`; absent, the page never posts it. */
  readonly finish?: { readonly status: number; readonly code?: string };
  /** The credential ids the person's machine-logins list holds at the start. */
  readonly listed?: readonly string[];
  /** Whether a revoke takes its id off the list, as the broker's revoke does. */
  readonly revokeRemoves?: boolean;
}

export interface OriginStandIn {
  /** `http://127.0.0.1:<port>`. */
  readonly url: string;
  /** Every id a revoke named, in order. */
  readonly revoked: readonly string[];
  stop(): void;
}

/** Dispatch's alert for a request that reached nobody (web/src/features/signin/refusals.ts). */
export const unreachedAlert = "Dispatch could not be reached. Try again.";

function signInPage(finish: OriginBehaviour["finish"]): string {
  const click =
    finish === undefined
      ? `say(${JSON.stringify(unreachedAlert)});`
      : `try {
          const answer = await fetch("/auth/passkey/login/finish", { method: "POST", body: "{}" });
          const body = await answer.json().catch(() => ({}));
          say(typeof body.error === "string" ? body.error : "answered " + answer.status);
        } catch {
          say(${JSON.stringify(unreachedAlert)});
        }`;
  return `<!doctype html><html><body><main>
<button type="button" id="sign-in">Sign in with a passkey</button>
<script>
function say(text) {
  const alert = document.createElement("div");
  alert.setAttribute("role", "alert");
  alert.textContent = text;
  document.querySelector("main").append(alert);
}
document.getElementById("sign-in").onclick = async () => {
  ${click}
};
</script></main></body></html>`;
}

const enrollPage = `<!doctype html><html><body><main>
<script>
const alert = document.createElement("div");
alert.setAttribute("role", "alert");
alert.textContent = "This link is not one Dispatch made: " + location.href;
document.querySelector("main").append(alert);
</script></main></body></html>`;

export function startOriginStandIn(behaviour: OriginBehaviour = {}): OriginStandIn {
  let listed = [...(behaviour.listed ?? [])];
  const revoked: string[] = [];
  const htmlHeaders = { "Content-Type": "text/html; charset=utf-8" };
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    fetch(request) {
      const { pathname } = new URL(request.url);
      if (pathname === "/auth/whoami") {
        return Response.json({ error: "no session" }, { status: 401 });
      }
      if (request.method === "GET" && pathname === "/") {
        return new Response(signInPage(behaviour.finish), { headers: htmlHeaders });
      }
      if (request.method === "GET" && pathname === "/enroll") {
        return new Response(enrollPage, { headers: htmlHeaders });
      }
      if (request.method === "POST" && pathname === "/auth/passkey/login/finish") {
        const finish = behaviour.finish;
        if (finish === undefined) return new Response("not here", { status: 404 });
        if (finish.code === undefined) return new Response("refused", { status: finish.status });
        return Response.json(
          { error: `the stand-in answers ${finish.code}`, code: finish.code },
          { status: finish.status }
        );
      }
      if (request.method === "GET" && pathname === "/api/v1/machine-logins") {
        return Response.json({ credentials: listed.map((id) => ({ credential_id: id })) });
      }
      const revoke = /^\/api\/v1\/machine-logins\/([^/]+)\/revoke$/.exec(pathname);
      if (request.method === "POST" && revoke !== null) {
        const id = decodeURIComponent(revoke[1] ?? "");
        revoked.push(id);
        if (!listed.includes(id)) return Response.json({ code: "NOT_FOUND" }, { status: 404 });
        if (behaviour.revokeRemoves === true) listed = listed.filter((one) => one !== id);
        return Response.json({ state: "revoked" });
      }
      return new Response("not here", { status: 404 });
    },
  });
  return {
    url: `http://127.0.0.1:${server.port}`,
    revoked,
    stop: () => server.stop(true),
  };
}
