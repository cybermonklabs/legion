// The router of the views demo's single file (packages/dispatch/e2e/views-demo/README.md). A page
// opened from the disk is at a `file:` address, where a browser lets the page change only the
// query and the fragment of its own address, so `vite.demo.config.ts` gives the app's entry this
// module in place of react-router-dom when it builds the single file: the app's BrowserRouter is a
// HashRouter there, and the page opens on the fixtures' project's Issues tab.
export { HashRouter as BrowserRouter } from "react-router-dom";

const project = window.__DISPATCH_DEMO_FIXTURES__?.manifest.project;
if (project !== undefined && (window.location.hash === "" || window.location.hash === "#/")) {
  window.location.replace(`#/projects/${project}/issues`);
}
