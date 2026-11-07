// PROTOTYPE (dispatch://INFRA-1508): the dev server against a `dispatch:local` instance on any
// port, named by DISPATCH_DEV_TARGET (e.g. http://127.0.0.1:43367), with hot reload.
import { mergeConfig, type Plugin } from "vite";

import base from "./vite.config";

const target = process.env.DISPATCH_DEV_TARGET ?? "http://localhost:8766";

/** A page load without a session the local server accepts goes through its dev sign-in first, so
 *  any prototype link opens signed in, whatever path it names, and a cookie left by an earlier
 *  server process (dev sessions are valid on one process only) is replaced. */
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
      response.statusCode = 302;
      response.setHeader(
        "location",
        `/auth/_dev/signin?login=sami@theorem.dev&next=${encodeURIComponent(url)}`
      );
      response.end();
    });
  },
};

export default mergeConfig(base, {
  plugins: [autoSignIn],
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
