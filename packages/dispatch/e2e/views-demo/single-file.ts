// The views demo as one HTML file (README.md beside this file), which a browser opens from the
// disk with no network: one Vite build of the demo (web/vite.demo.config.ts with
// DISPATCH_DEMO_SINGLE_FILE=1, so one script, one stylesheet and every asset inlined), then the
// script, the stylesheet, the icon and the fixtures written into the page itself.
//
//   bun e2e/views-demo/single-file.ts --fixtures <dir> --out <file.html> [--build-dir <dir>]
//
// The stub patches fetch and WebSocket in the page (web/src/demo/install.ts), which needs no
// service worker, and the app's router reads the address's fragment (web/src/demo/file-router.ts),
// since a page at a `file:` address may change nothing else of it.
import { mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { FIXTURE_NAMES } from "../../web/src/demo/fixtures";

const { values: flags } = parseArgs({
  options: {
    fixtures: { type: "string" },
    out: { type: "string" },
    "build-dir": { type: "string" },
  },
});
if (flags.fixtures === undefined || flags.out === undefined) {
  throw new Error("name --fixtures <dir> and --out <file.html>");
}
const fixtures = flags.fixtures;
const web = join(import.meta.dir, "..", "..", "web");
const buildDir = flags["build-dir"] ?? (await mkdtemp(join(tmpdir(), "views-demo-build-")));
const dist = join(buildDir, "dist");

const build = Bun.spawn(
  ["bunx", "vite", "build", "--config", "vite.demo.config.ts", "--outDir", dist, "--emptyOutDir"],
  {
    cwd: web,
    env: { ...process.env, DISPATCH_DEMO_SINGLE_FILE: "1", DISPATCH_DEMO_FIXTURES: fixtures },
    stdout: "inherit",
    stderr: "inherit",
  }
);
if ((await build.exited) !== 0) throw new Error("the demo's Vite build failed");

/** The files the stub loads (web/src/demo/fixtures.ts), by name. */
const loaded: Record<string, unknown> = {};
for (const name of FIXTURE_NAMES) {
  loaded[name] = JSON.parse(await readFile(join(fixtures, `${name}.json`), "utf8"));
}

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
// The fixtures go first, so the stub finds them before it runs.
const data = JSON.stringify(loaded).replace(/</g, "\\u003c");
html = html.replace(
  "<head>",
  () => `<head>\n<script>window.__DISPATCH_DEMO_FIXTURES__ = ${data};</script>`
);
const external = [...html.matchAll(/(?:src|href)="(\/assets\/[^"]+)"/g)].map((match) => match[1]);
if (external.length > 0) throw new Error(`the page still names files: ${external.join(", ")}`);

await writeFile(flags.out, html);
const size = (await stat(flags.out)).size;
console.log(`wrote ${flags.out}: ${(size / 1024 / 1024).toFixed(1)} MiB`);
if (flags["build-dir"] === undefined) await rm(buildDir, { recursive: true });
