// The views demo's banner: a small fixed note at the foot of every page that says the back end is
// stubbed, which data is real and when it was read, and which figures are estimated or invented.
// It sits outside the app's root and takes no pointer events, so it moves no part of the layout
// and blocks no control. On a phone it is one short line, so it covers no first row.
import type { DemoManifest } from "./fixtures";

/** A UTC time as `14 Nov 00:36Z`. */
function utc(at: string): string {
  const date = new Date(at);
  const month = date.toLocaleString("en-GB", { month: "short", timeZone: "UTC" });
  return `${date.getUTCDate()} ${month} ${date.toISOString().slice(11, 16)}Z`;
}

const STYLE = `
.views-demo-banner {
  position: fixed; left: 8px; bottom: 8px; z-index: 2147483647; pointer-events: none;
  max-width: min(400px, calc(100vw - 16px)); padding: 4px 8px; border-radius: 6px;
  background: rgba(120, 53, 15, 0.9); color: #fff7ed; font: 11px/1.35 system-ui, sans-serif;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.3);
}
.views-demo-banner .short { display: none; }
.views-demo-banner b { font-weight: 600; }
@media (max-width: 640px) {
  .views-demo-banner { left: auto; right: 4px; bottom: 4px; padding: 2px 6px; font-size: 10px; }
  .views-demo-banner .full { display: none; }
  .views-demo-banner .short { display: block; }
}`;

export function showBanner(manifest: DemoManifest): void {
  const style = document.createElement("style");
  style.textContent = STYLE;
  document.head.append(style);
  const banner = document.createElement("aside");
  banner.className = "views-demo-banner";
  banner.setAttribute("aria-label", "Demo data");
  banner.dataset.testid = "views-demo-banner";
  const full = document.createElement("div");
  full.className = "full";
  const lines = [
    "Demo: the back end is stubbed",
    `Real: ${manifest.project} issues (read ${utc(manifest.issues_read_at)}), Legion daemon state (read ${utc(manifest.daemon_read_at)})`,
    `Estimated from issue events: ${manifest.derived_summary.join(", ")}`,
    `Invented: ${manifest.invented_summary.join(", ")}`,
  ];
  for (const [index, text] of lines.entries()) {
    const line = document.createElement(index === 0 ? "b" : "div");
    line.textContent = text;
    full.append(line);
  }
  const short = document.createElement("div");
  short.className = "short";
  short.textContent = `Stubbed back end · real ${manifest.project} issues and daemon state, read ${utc(manifest.daemon_read_at)} · phase times and rounds estimated, Legion setting invented`;
  banner.append(full, short);
  document.body.append(banner);
}
