// The webkit-check stage of packages/daemon/docker/worker.Dockerfile runs this as the image's
// runtime user, `node webkit-launch.mjs <playwright-core directory>`, once that stage has
// downloaded the WebKit build of the playwright-core it names: the Playwright packages/dispatch
// locks. It launches that WebKit once, headless, through Playwright's own launch, which validates
// the host before it starts the browser, so an image whose runtime-base apt line lacks a library
// this WebKit needs fails its build. Any error propagates: the rejection exits non-zero with
// Playwright's own message, which for a missing library is "Host system is missing dependencies to
// run browsers." followed by the libraries it found missing.
import { createRequire } from "node:module";
import path from "node:path";

if (process.argv.length !== 3) {
  throw new Error("usage: node webkit-launch.mjs <playwright-core directory>");
}
const core = path.resolve(process.argv[2]);
const require = createRequire(import.meta.url);
const { version } = require(path.join(core, "package.json"));
const { webkit } = require(core);
const browser = await webkit.launch();
try {
  console.log(`webkit: Playwright ${version} launched WebKit ${browser.version()}`);
} finally {
  await browser.close();
}
