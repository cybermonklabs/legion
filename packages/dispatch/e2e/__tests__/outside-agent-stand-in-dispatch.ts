/** The stand-in Dispatch the tailnet leg's tests run against, as a process of its own, so a test
 *  worker's event loop that something holds (the bounded runner's synchronous stop of a sibling
 *  run) never holds its answers. `outside-agent-tailnet-stand-ins.ts` starts it with its settings
 *  in the environment, `OUTSIDE_STAND_IN_DISPATCH` (JSON): a nonce, the state directory, whether
 *  the full member's controls are answered or refused, the commits /healthz names, the full
 *  member's login and the repository of the GitHub proxy's read.
 *
 *  It binds a port of the kernel's choosing (port 0) and writes it to `<state>/dispatch-port`, so
 *  no port a parent chose and closed can be taken by another process before it binds. It answers
 *  `GET /_ready` (unrecorded) with the nonce, so the parent knows the answer is its own child's
 *  and not another run's stand-in's. Its /healthz names the commit and fleet_commit the settings
 *  say. It answers the full member's browser session (cookie `session=full`, with the origin's own
 *  Origin) on the routes the leg's controls read, and 401 to anything else. Its access list names
 *  the test person until `POST /_remove`, the owner's removal. It appends each request it answers,
 *  `METHOD path`, to `<state>/dispatch-seen`. */

import { appendFileSync, renameSync, writeFileSync } from "node:fs";
import { join } from "node:path";

interface Settings {
  readonly nonce: string;
  readonly state: string;
  readonly controls: "answered" | "refused";
  readonly commit: string;
  readonly fleetCommit: string;
  readonly fullLogin: string;
  readonly proxyRepository: string;
}

const settings = JSON.parse(process.env.OUTSIDE_STAND_IN_DISPATCH ?? "{}") as Settings;
const seen = join(settings.state, "dispatch-seen");
let listed = true;

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
      return Response.json({});
    }
    if (
      request.headers.get("cookie") !== "session=full" ||
      request.headers.get("origin") !== url.origin
    ) {
      return Response.json({ code: "UNAUTHENTICATED" }, { status: 401 });
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
      return Response.json({ people: listed ? ["outside-e2e@example.com"] : [] });
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
    if (route === `GET /api/github/rest/repos/${settings.proxyRepository}/actions/runs`) {
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
