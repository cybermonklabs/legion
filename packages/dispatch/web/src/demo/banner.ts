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
  .views-demo-banner {
    left: 4px; right: 4px; bottom: 4px; max-width: none; padding: 2px 6px;
    font-size: 9px; line-height: 1.3;
  }
  .views-demo-banner .full { display: none; }
  .views-demo-banner .short { display: block; }
}`;

/** The banner, of the manifest's words; `failures` adds the failed reads the tab asked for
 *  (install.ts), which no production read holds either. */
export function showBanner(manifest: DemoManifest, failures: readonly string[]): void {
  // Each project's issues with the time they were read; the daemon's state is the first one's.
  const projects = [
    `${manifest.project} issues (read ${utc(manifest.issues_read_at)})`,
    ...manifest.no_daemon_projects.map(
      ({ key, read_at }) => `${key} issues (read ${utc(read_at)})`
    ),
  ].join(", ");
  const invented = [...manifest.invented_summary, ...failures].join(", ");
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
    `Real: ${projects}, ${manifest.project}'s Legion daemon state (read ${utc(manifest.daemon_read_at)})`,
    `Estimated from issue events: ${manifest.derived_summary.join(", ")}`,
    `Invented: ${invented}`,
  ];
  for (const [index, text] of lines.entries()) {
    const line = document.createElement(index === 0 ? "b" : "div");
    line.textContent = text;
    full.append(line);
  }
  const short = document.createElement("div");
  short.className = "short";
  // The phone's two lines: the invented figures in the manifest's own words, and the read time;
  // the real projects' names are the desktop banner's, since they would take a third line.
  short.textContent = `Stubbed · real data of ${utc(manifest.daemon_read_at)}, times estimated · invented: ${invented}`;
  banner.append(full, short);
  document.body.append(banner);
}
