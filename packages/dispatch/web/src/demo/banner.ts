// The ask card demo's banner: a small fixed note in the bottom right corner of every page that
// says the back end is stubbed and the text invented. It sits outside the app's root and takes no
// pointer events, so it moves no part of the layout and blocks no control.
const STYLE = `
.ask-card-demo-banner {
  position: fixed; right: 8px; bottom: 8px; z-index: 2147483647; pointer-events: none;
  padding: 3px 8px; border-radius: 6px; background: rgba(120, 53, 15, 0.88); color: #fff7ed;
  font: 500 11px/1.35 Inter, ui-sans-serif, system-ui, sans-serif; white-space: nowrap;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.25);
}
@media (max-width: 640px) {
  .ask-card-demo-banner { right: 4px; bottom: 4px; padding: 2px 6px; font-size: 9px; }
}`;

export function showBanner(): void {
  const style = document.createElement("style");
  style.textContent = STYLE;
  document.head.append(style);
  const banner = document.createElement("aside");
  banner.className = "ask-card-demo-banner";
  banner.setAttribute("aria-label", "Demo");
  banner.dataset.testid = "ask-card-demo-banner";
  banner.textContent = "Prototype · stubbed back end · invented text";
  document.body.append(banner);
}
