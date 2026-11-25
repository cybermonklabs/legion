/** The stand-in Dispatch the tailnet leg's tests run against, as a process of its own, so a test
 *  worker's event loop that something holds never holds its answers.
 *  `outside-agent-tailnet-stand-ins.ts` starts it with its settings
 *  in the environment, `OUTSIDE_STAND_IN_DISPATCH` (JSON): a nonce, the state directory, whether
 *  the full member's controls are answered or refused, the commits /healthz names, the full
 *  member's login and the repository of the GitHub proxy's read.
 *
 *  It binds a port of the kernel's choosing (port 0) and writes it to `<state>/dispatch-port`, so
 *  no port a parent chose and closed can be taken by another process before it binds. It answers
 *  `GET /_ready` (unrecorded) with the nonce, so the parent knows the answer is its own child's
 *  and not another run's stand-in's. Its /healthz names the commit and fleet_commit the settings
 *  say. It answers the full member's browser session (cookie `session=full`) on the routes the
 *  leg's controls read, and refuses that cookie's write whose Origin is not its own origin, 403
 *  `invalid request origin`, as Dispatch's router does; it answers the full member's personal
 *  token (`Authorization: Bearer <fullToken>`) on whoami, the credential-grants list
 *  (`<state>/full-grants.json`) and the GitHub proxy's read, which `<state>/proxy-refuses-token`
 *  makes 403 OAUTH_SCOPE_REQUIRED; and 401 to anything else. Its access list names the test person
 *  until `POST /_remove`, the owner's removal, which leaves `<state>/removed` (the stand-in public
 *  leg's window ends on it); in a browser gate's run (`<state>/browser`) the list
 *  is `<state>/listed`, which the owner's routes by cookie change: PUT and DELETE
 *  `/api/v1/projects/EXAMPLE/access/people/<address>` (DELETE leaves `<state>/removed`) and POST
 *  `.../invitations`, which makes a link (kept in `<state>/links`) for an address the list names
 *  and answers 409 PERSON_NOT_LISTED for any other. It appends each request it answers,
 *  `METHOD path`, to `<state>/dispatch-seen`. */

import { appendFileSync, existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { join } from "node:path";

interface Settings {
  readonly nonce: string;
  readonly state: string;
  readonly controls: "answered" | "refused";
  readonly commit: string;
  readonly fleetCommit: string;
  readonly fullLogin: string;
  readonly fullToken: string;
  readonly linkToken: string;
  readonly proxyRepository: string;
}

const settings = JSON.parse(process.env.OUTSIDE_STAND_IN_DISPATCH ?? "{}") as Settings;
const seen = join(settings.state, "dispatch-seen");
let listed = true;
const listFile = join(settings.state, "listed");
const browserGate = existsSync(join(settings.state, "browser"));
/** The people the access list names now. */
const people = (): string[] =>
  browserGate
    ? readFileSync(listFile, "utf8")
        .split("\n")
        .filter((line) => line !== "")
    : listed
      ? ["outside-e2e@example.com"]
      : [];

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    const path = url.pathname;
    if (path === "/_ready") return new Response(settings.nonce);
    appendFileSync(seen, `${request.method} ${path}\n`);
    if (path === "/healthz") {
      return Response.json({
        ok: true,
        commit: settings.commit,
        fleet_commit: settings.fleetCommit,
      });
    }
    if (path === "/_remove") {
      listed = false;
      writeFileSync(join(settings.state, "removed"), "");
      return Response.json({});
    }
    const proxyRuns = `GET /api/github/rest/repos/${settings.proxyRepository}/actions/runs`;
    if (request.headers.get("authorization") === `Bearer ${settings.fullToken}`) {
      const route = `${request.method} ${path}`;
      if (route === "GET /api/v1/whoami") {
        return Response.json({ kind: "user", login: settings.fullLogin, scopes: ["person"] });
      }
      if (route === "GET /api/v1/credential-grants" && url.searchParams.get("approver") === "me") {
        return new Response(readFileSync(join(settings.state, "full-grants.json")), {
          headers: { "Content-Type": "application/json" },
        });
      }
      if (route === proxyRuns) {
        return existsSync(join(settings.state, "proxy-refuses-token"))
          ? Response.json({ code: "OAUTH_SCOPE_REQUIRED" }, { status: 403 })
          : Response.json({ total_count: 0, workflow_runs: [] });
      }
      return Response.json({ code: "UNAUTHENTICATED" }, { status: 401 });
    }
    if (request.headers.get("cookie") !== "session=full") {
      return Response.json({ code: "UNAUTHENTICATED" }, { status: 401 });
    }
    if (request.method !== "GET" && request.headers.get("origin") !== url.origin) {
      return Response.json({ error: "invalid request origin" }, { status: 403 });
    }
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    const code = typeof body.code === "string" ? body.code : "";
    const route = `${request.method} ${path}`;
    if (route === "GET /api/v1/whoami") {
      return Response.json({
        kind: "user",
        login: settings.fullLogin,
        scopes: ["person", "approve"],
      });
    }
    if (route === "GET /api/v1/projects/EXAMPLE/access") {
      return Response.json({ people: people() });
    }
    const person = /^\/api\/v1\/projects\/EXAMPLE\/access\/people\/([^/]+)(\/invitations)?$/.exec(
      path
    );
    if (browserGate && person !== null) {
      const address = decodeURIComponent(person[1] ?? "");
      const named = people().includes(address);
      if (person[2] !== undefined && request.method === "POST") {
        if (!named) return Response.json({ code: "PERSON_NOT_LISTED" }, { status: 409 });
        const link = `https://dispatch.edge.example/enroll#${settings.linkToken}-${crypto.randomUUID()}`;
        appendFileSync(join(settings.state, "links"), `${link}\n`);
        return Response.json(
          { url: link, made_at: "2026-01-01T00:00:00Z", expires_at: "2026-01-02T00:00:00Z" },
          { status: 201 }
        );
      }
      if (person[2] === undefined && request.method === "PUT") {
        if (!named) appendFileSync(listFile, `${address}\n`);
        return Response.json({ people: people() });
      }
      if (person[2] === undefined && request.method === "DELETE") {
        const kept = people().filter((each) => each !== address);
        writeFileSync(listFile, kept.map((each) => `${each}\n`).join(""));
        writeFileSync(join(settings.state, "removed"), "");
        return Response.json({ people: kept });
      }
    }
    if (route === "POST /api/v1/me/agent-tokens") {
      return Response.json(
        { id: "token-1", name: body.name, token: "dsp_plaintext" },
        { status: 201 }
      );
    }
    if (route === "DELETE /api/v1/me/agent-tokens/token-1")
      return new Response(null, { status: 204 });
    if (route === "POST /api/v1/credential-requests/machine-lookup") {
      return Response.json({ record_id: `record-${code}` });
    }
    if (route === `POST /api/v1/credential-requests/record-${code}/approve`) {
      if (settings.controls === "refused") {
        return Response.json({ code: "FORBIDDEN" }, { status: 403 });
      }
      writeFileSync(join(settings.state, `approved-${code}`), "");
      return Response.json({ credential_id: `cred-${code.toLowerCase()}` });
    }
    if (route.startsWith("POST /api/v1/machine-logins/cred-")) return Response.json({});
    if (route === proxyRuns) {
      return settings.controls === "answered"
        ? Response.json({ total_count: 0, workflow_runs: [] })
        : Response.json({ code: "GITHUB_TOKEN_UNAVAILABLE" }, { status: 503 });
    }
    return Response.json({ code: "NOT_FOUND" }, { status: 404 });
  },
});
// The port, written whole: a reader sees no file or the whole number, never a part of it.
writeFileSync(join(settings.state, "dispatch-port.writing"), String(server.port));
renameSync(join(settings.state, "dispatch-port.writing"), join(settings.state, "dispatch-port"));
