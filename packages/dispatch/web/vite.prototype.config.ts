// PROTOTYPE (dispatch://INFRA-1508): the dev server against a `dispatch:local` instance on any
// port, named by DISPATCH_DEV_TARGET (e.g. http://127.0.0.1:43367), with hot reload.
import { mergeConfig } from "vite";

import base from "./vite.config";

const target = process.env.DISPATCH_DEV_TARGET ?? "http://localhost:8766";

export default mergeConfig(base, {
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
