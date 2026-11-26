import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";

// Two aliases, each naming a directory, read by the walk of e2e/tools/today-page-modules.ts as it
// reads web/vite.config.ts's (e2e/__tests__/today-page-modules.test.ts says what each one reaches).
export default defineConfig({
  resolve: {
    alias: {
      "@fixture/contracts": fileURLToPath(new URL("../contracts/src", import.meta.url)),
      "@fixture/editor": fileURLToPath(new URL("../editor/src", import.meta.url)),
    },
  },
});
