// PROTOTYPE (dispatch://INFRA-1508): the dev server against a `dispatch:local` instance on any
// port, named by DISPATCH_DEV_TARGET (e.g. http://127.0.0.1:43367), with hot reload.
import { readFile } from "node:fs/promises";

import { mergeConfig, type Plugin } from "vite";

import base from "./vite.config";

const target = process.env.DISPATCH_DEV_TARGET ?? "http://localhost:8766";

/** A page load without a session the local server accepts gets a small page that signs in through
 *  the local server's dev sign-in and then reloads itself. Not a redirect: Dispatch's session
 *  cookie is SameSite=Strict, so on a redirect chain another site started (a link clicked in the
 *  Dispatch Inbox) the browser never sends it and the check would redirect forever. A reload the
 *  page itself starts is same-site, so the cookie goes with it. The page signs in once; if the
 *  session still does not take, it says so instead of looping. */
const autoSignIn: Plugin = {
  name: "prototype-auto-sign-in",
  configureServer(server) {
    server.middlewares.use(async (request, response, next) => {
      const url = request.url ?? "/";
      const page = request.method === "GET" && (request.headers.accept ?? "").includes("text/html");
      if (!page || /^\/(auth|api|ws|@|src\/|node_modules\/)/.test(url)) return next();
      const whoami = await fetch(`${target}/auth/whoami`, {
        headers: { cookie: request.headers.cookie ?? "" },
      }).catch(() => undefined);
      if (whoami?.ok) return next();
      response.statusCode = 200;
      response.setHeader("content-type", "text/html; charset=utf-8");
      response.setHeader("cache-control", "no-store");
      response.end(`<!doctype html><meta charset="utf-8"><title>Signing in…</title>
<p style="font:16px system-ui;margin:3em">Signing in to the local Dispatch…</p>
<script>
(async () => {
  // Reached again within 3 s means the navigation did not carry the session: say so, never loop.
  const last = Number(sessionStorage.getItem("prototype-signin") ?? 0);
  if (Date.now() - last < 3000) {
    document.querySelector("p").textContent =
      "Signing in to the local Dispatch did not take. Reload to try again.";
    return;
  }
  await fetch("/auth/_dev/signin?login=sami@theorem.dev&next=/", { redirect: "manual" });
  if (!(await fetch("/auth/whoami")).ok) {
    document.querySelector("p").textContent =
      "The local Dispatch did not sign you in. Reload to try again.";
    return;
  }
  sessionStorage.setItem("prototype-signin", String(Date.now()));
  location.replace(location.href);
})();
</script>`);
    });
  },
};

/** The Legion phase of each issue of the local copy, from a daemon-state snapshot, until Dispatch
 *  reads the daemon itself (dispatch://INFRA-1077). DISPATCH_PROTOTYPE_LEGION_PHASES names the
 *  file; with none, the table shows no phase. */
const legionPhases: Plugin = {
  name: "prototype-legion-phases",
  configureServer(server) {
    server.middlewares.use("/prototype/legion-phases.json", async (_request, response) => {
      const file = process.env.DISPATCH_PROTOTYPE_LEGION_PHASES;
      const body = file ? await readFile(file, "utf8").catch(() => undefined) : undefined;
      response.statusCode = body === undefined ? 404 : 200;
      response.setHeader("content-type", "application/json");
      response.end(body ?? "{}");
    });
  },
};

export default mergeConfig(base, {
  plugins: [autoSignIn, legionPhases],
  server: {
    port: 5180,
    strictPort: true,
    // Dispatch refuses a request whose Host or Origin is not its own, so the proxy sends its.
    proxy: {
      "/api": { target, changeOrigin: true, headers: { origin: target } },
      "/auth": { target, changeOrigin: true, headers: { origin: target } },
      "/ws": {
        target: target.replace(/^http/, "ws"),
        ws: true,
        changeOrigin: true,
        headers: { origin: target },
      },
    },
  },
});
