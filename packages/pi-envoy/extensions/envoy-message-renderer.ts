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
 * A card longer than the fold, as a collapsed card shows it: its first lines, then the lines of
 * the words its sender wrote that are past them (`readInboundCard`), with a "…" for each run of
 * lines left out. A Dispatch message's card writes its `body` block after a header longer than the
 * fold, and an agent's message whose card carries many fields (a kept message, a reply asked for,
 * the message it answers) opens its `message` block below it too: folded at the fold alone, either
 * showed none of the words it brings until Ctrl+O (dispatch://INFRA-5271). A block holds the words
 * line by line, so each of their lines is shown, as a field shows all of them on its one line.
 */
function collapsed(text: string): string {
  const lines = text.split("\n");
  if (lines.length <= ENVOY_MESSAGE_COLLAPSED_LINES) return text;
  const head = lines.slice(0, ENVOY_MESSAGE_COLLAPSED_LINES);
  const words = readInboundCard(text)?.words;
  const end = words === undefined ? 0 : words.line + words.lines;
  if (words === undefined || end <= ENVOY_MESSAGE_COLLAPSED_LINES) return `${head.join("\n")}\n…`;
  const from = Math.max(words.line, ENVOY_MESSAGE_COLLAPSED_LINES);
  const skipped = from > ENVOY_MESSAGE_COLLAPSED_LINES ? ["…"] : [];
  const after = end < lines.length ? ["…"] : [];
  return [...head, ...skipped, ...lines.slice(from, end), ...after].join("\n");
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
