// The router of the ask card demo's single file (packages/dispatch/e2e/ask-card-demo/README.md).
// A page opened from the disk is at a `file:` address, where a browser lets the page change only
// the query and the fragment of its own address, so `vite.demo.config.ts` gives the app's entry
// this module in place of react-router-dom when it builds the single file: the app's
// BrowserRouter is a HashRouter there, and the page opens on the Inbox, the app's `/`.
export { HashRouter as BrowserRouter } from "react-router-dom";

if (window.location.hash === "") window.location.replace("#/");
