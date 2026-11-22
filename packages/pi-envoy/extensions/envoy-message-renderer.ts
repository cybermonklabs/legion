import { readInboundCard } from "@legion/envoy-client/inbound-card";
import type {
  MessageRendererTheme,
  MessageRenderOptions,
  PiApi,
  RenderableMessage,
} from "@legion/pi-shared/pi-types";
import { Box, Spacer, Text } from "@oh-my-pi/pi-tui";

/**
 * Lines of the inbound body shown before the "…" fold when collapsed. Kept
 * small since these cards accumulate in a live transcript.
 */
const ENVOY_MESSAGE_COLLAPSED_LINES = 12;

/**
 * A card longer than the fold, as a collapsed card shows it: its first lines, then the line that
 * holds the words its sender wrote when that line is past them (`readInboundCard`), with a "…"
 * for each run of lines left out. A Dispatch message's card writes its `body` after a header
 * longer than the fold, and so does an agent's message whose sender holds a role, under its
 * `reply_role`: folded at the fold alone, either showed none of the words it brings until
 * Ctrl+O (dispatch://INFRA-5271).
 */
function collapsed(text: string): string {
  const lines = text.split("\n");
  if (lines.length <= ENVOY_MESSAGE_COLLAPSED_LINES) return text;
  const head = lines.slice(0, ENVOY_MESSAGE_COLLAPSED_LINES);
  const words = readInboundCard(text)?.words?.line;
  if (words === undefined || words < ENVOY_MESSAGE_COLLAPSED_LINES) return `${head.join("\n")}\n…`;
  const skipped = words > ENVOY_MESSAGE_COLLAPSED_LINES ? ["…"] : [];
  const after = words < lines.length - 1 ? ["…"] : [];
  return [...head, ...skipped, ...lines.slice(words, words + 1), ...after].join("\n");
}

/**
 * Renders an inbound `envoy-message` as a plain-text card instead of OMP's
 * default Markdown body. `renderInbound` (`@legion/envoy-client/delivery`)
 * nests a structured `message` value under a `message:` TOON key at 4-space
 * indent, which CommonMark reads as an indented code block — Markdown then
 * mangles or mis-styles every delivery. Displaying the content through
 * `Text` (not `Markdown`) keeps it verbatim.
 */
function renderEnvoyMessage(
  message: RenderableMessage,
  options: MessageRenderOptions,
  theme: MessageRendererTheme
): Box {
  const box = new Box(1, 1, undefined, {
    chars: theme.boxRound,
    color: (text) => theme.fg("borderMuted", text),
  });
  box.setIgnoreTight(true);
  box.addChild(new Text(theme.fg("customMessageLabel", theme.bold(message.customType)), 0, 0));
  box.addChild(new Spacer(1));

  // A delivery that carries pictures is its text block, then one image block per picture.
  const text =
    typeof message.content === "string"
      ? message.content
      : message.content
          .map((block) => (block.type === "text" ? block.text : `[picture: ${block.mimeType}]`))
          .join("\n");
  box.addChild(
    new Text(theme.fg("customMessageText", options.expanded ? text : collapsed(text)), 0, 0)
  );

  return box;
}

export function registerEnvoyMessageRenderer(pi: Pick<PiApi, "registerMessageRenderer">): void {
  pi.registerMessageRenderer("envoy-message", renderEnvoyMessage);
}
