/**
 * A reader of a `text/event-stream` body, as the HTML standard frames one: lines ending in LF,
 * CR or CRLF, each `field: value` (one space after the colon dropped, a line starting `:` a
 * comment), and a blank line dispatching the event the lines before it built. `data:` lines join
 * with a newline; `id:` sets the id every later event carries until another replaces it; an event
 * with no `event:` field is named `message`. The page reads Dispatch's answer stream
 * (`GET /api/v1/going/requests/{id}/stream`) through it rather than through `EventSource`, which
 * sends no header of the page's own and cannot be aborted as one request of the client.
 */

/**
 * A decoder fed the stream's text piece by piece, in whatever pieces the network delivers: a
 * line split across two pieces, and an event across many, is dispatched once its blank line
 * arrives. A CR ending one piece is held until the next shows whether an LF follows it, so a CRLF
 * split across two pieces is one line break and not two; `done` says no piece follows, which
 * makes a CR held at the very end the line break it was.
 */
export function eventStreamDecoder(
  onEvent: (event: {
    readonly id: string | undefined;
    readonly name: string;
    readonly data: string;
  }) => void
): (text: string, done?: boolean) => void {
  let buffer = "";
  let id: string | undefined;
  let name = "";
  let data: string[] = [];
  const line = (text: string) => {
    if (text === "") {
      if (data.length > 0) {
        onEvent({ data: data.join("\n"), id, name: name === "" ? "message" : name });
      }
      name = "";
      data = [];
      return;
    }
    if (text.startsWith(":")) {
      return;
    }
    const colon = text.indexOf(":");
    const field = colon === -1 ? text : text.slice(0, colon);
    const raw = colon === -1 ? "" : text.slice(colon + 1);
    const value = raw.startsWith(" ") ? raw.slice(1) : raw;
    if (field === "data") {
      data.push(value);
    } else if (field === "event") {
      name = value;
    } else if (field === "id" && !value.includes("\0")) {
      id = value;
    }
  };
  return (text, done = false) => {
    buffer += text;
    for (;;) {
      const breakAt = buffer.search(/[\r\n]/);
      if (breakAt === -1) {
        return;
      }
      if (buffer[breakAt] === "\r" && breakAt === buffer.length - 1 && !done) {
        // A CR at the very end of what has arrived: the next piece says whether it is a CRLF.
        return;
      }
      const width = buffer.startsWith("\r\n", breakAt) ? 2 : 1;
      line(buffer.slice(0, breakAt));
      buffer = buffer.slice(breakAt + width);
    }
  };
}

/**
 * Reads `body` to its end, handing each event to `onEvent` as its blank line arrives; `onEvent`
 * answers `true` to stop reading there, which releases the body. Resolves `true` when it was
 * stopped and `false` when the body ended first; a body that fails rejects with its failure.
 */
export async function readEventStream(
  body: ReadableStream<Uint8Array>,
  onEvent: (event: {
    readonly id: string | undefined;
    readonly name: string;
    readonly data: string;
  }) => boolean
): Promise<boolean> {
  const reader = body.getReader();
  const text = new TextDecoder();
  let stopped = false;
  const decode = eventStreamDecoder((event) => {
    if (!stopped && onEvent(event)) {
      stopped = true;
    }
  });
  try {
    while (!stopped) {
      const piece = await reader.read();
      if (piece.done) {
        decode(text.decode(), true);
        return stopped;
      }
      decode(text.decode(piece.value, { stream: true }));
    }
    return true;
  } finally {
    if (stopped) {
      await reader.cancel().catch(() => undefined);
    }
    reader.releaseLock();
  }
}
