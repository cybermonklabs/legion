// The ask card demo as one HTML file (README.md beside this file), which a browser opens from the
// disk with no network: one Vite build of the demo (web/vite.demo.config.ts with
// DISPATCH_DEMO_SINGLE_FILE=1, so one script, one stylesheet and every asset inlined), then the
// script, the stylesheet and the icon written into the page itself. The fixtures are invented and
// built into the stub's own module, so nothing else goes in.
//
//   bun e2e/ask-card-demo/single-file.ts --out <file.html> [--build-dir <dir>]
//
// The stub patches fetch and WebSocket in the page (web/src/demo/install.ts), which needs no
// service worker, and the app's router reads the address's fragment (web/src/demo/file-router.ts),
// since a page at a `file:` address may change nothing else of it.
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";

const { values: flags } = parseArgs({
  options: {
    out: { type: "string" },
    "build-dir": { type: "string" },
  },
});
if (flags.out === undefined) throw new Error("name --out <file.html>");
const web = join(import.meta.dir, "..", "..", "web");
const scratch = process.env.TMPDIR ?? join(import.meta.dir, "..", "..", ".cache");
const buildDir = flags["build-dir"] ?? (await mkdtemp(join(scratch, "ask-card-demo-build-")));
const dist = join(buildDir, "dist");

const build = Bun.spawn(
  ["bunx", "vite", "build", "--config", "vite.demo.config.ts", "--outDir", dist, "--emptyOutDir"],
  {
    cwd: web,
    env: { ...process.env, DISPATCH_DEMO_SINGLE_FILE: "1" },
    stdout: "inherit",
    stderr: "inherit",
  }
);
if ((await build.exited) !== 0) throw new Error("the demo's Vite build failed");

/** Text that sits inside a <script> element: no `</script` can end it early. */
const inScript = (text: string) => text.replace(/<\/(script)/gi, "<\\/$1");

let html = await readFile(join(dist, "index.html"), "utf8");
const scripts = [...html.matchAll(/<script type="module" crossorigin src="([^"]+)"><\/script>/g)];
const styles = [...html.matchAll(/<link rel="stylesheet" crossorigin href="([^"]+)">/g)];
if (scripts.length !== 1) throw new Error(`expected one entry script, found ${scripts.length}`);
for (const [tag, src] of scripts) {
  const code = await readFile(join(dist, src), "utf8");
  html = html.replace(tag, () => `<script type="module">${inScript(code)}</script>`);
}
for (const [tag, href] of styles) {
  const css = await readFile(join(dist, href), "utf8");
  html = html.replace(tag, () => `<style>${css.replace(/<\/(style)/gi, "<\\/$1")}</style>`);
}
const icon = await readFile(join(dist, "favicon.svg"), "utf8").catch(() => undefined);
if (icon !== undefined) {
  const data = `data:image/svg+xml;base64,${Buffer.from(icon).toString("base64")}`;
  html = html.replace(/href="\/favicon\.svg"/, `href="${data}"`);
}
const external = [...html.matchAll(/(?:src|href)="(\/assets\/[^"]+)"/g)].map((match) => match[1]);
if (external.length > 0) throw new Error(`the page still names files: ${external.join(", ")}`);

await writeFile(flags.out, html);
const size = (await stat(flags.out)).size;
console.log(`wrote ${flags.out}: ${(size / 1024 / 1024).toFixed(1)} MiB`);
if (flags["build-dir"] === undefined) await rm(buildDir, { recursive: true });
