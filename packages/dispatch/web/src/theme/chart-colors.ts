/**
 * The axis, axis-label and split-line colours of a chart drawn on a canvas: values, not Tailwind
 * classes, since a canvas reads no stylesheet; a dark palette and its light counterpart for a
 * light colour scheme. A module importing nothing, so each chart reads it without pulling another
 * chart's module, and the chunk that module ships in, into its own.
 */
export const CHART_AXIS_COLORS = {
  dark: { axis: "#334155", label: "#94a3b8", split: "#1e293b" },
  light: { axis: "#cbd5e1", label: "#64748b", split: "#e2e8f0" },
};
