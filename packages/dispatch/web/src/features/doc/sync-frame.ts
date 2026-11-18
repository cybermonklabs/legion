import { MessageType } from "@hocuspocus/provider";
import { DOCUMENT_CLIENT_ID_MESSAGE_PREFIX } from "@legion/contracts";
import { createDecoder, type Decoder, readVarUint } from "lib0/decoding";

/**
 * Reads one length-prefixed field of a Hocuspocus frame as a view of the frame, and throws when
 * the length claims more bytes than the frame has left. A frame is often a view into a larger
 * buffer, so the bound is the frame's own length, never its buffer's.
 */
export function readField(decoder: Decoder): Uint8Array {
  const length = readVarUint(decoder);
  const start = decoder.pos;
  decoder.pos += length;
  if (decoder.pos > decoder.arr.length) {
    throw new RangeError("truncated document sync frame");
  }
  return decoder.arr.subarray(start, decoder.pos);
}

/**
 * The client id a document socket's first frame gives this page: the Hocuspocus stateless message
 * Dispatch writes before any other frame on a socket whose page presented
 * `DOCUMENT_CLIENT_ID_PARAMETER`, `DOCUMENT_CLIENT_ID_MESSAGE_PREFIX` and then the id in decimal.
 * `undefined` for any other frame, and for one that does not parse: a server of a build from
 * before Dispatch gave client ids out sends none.
 */
export function readClientIdMessage(frame: Uint8Array): number | undefined {
  try {
    const decoder = createDecoder(frame);
    readField(decoder); // the document name
    if (readVarUint(decoder) !== MessageType.Stateless) {
      return undefined;
    }
    const payload = new TextDecoder().decode(readField(decoder));
    if (!payload.startsWith(DOCUMENT_CLIENT_ID_MESSAGE_PREFIX)) {
      return undefined;
    }
    const digits = payload.slice(DOCUMENT_CLIENT_ID_MESSAGE_PREFIX.length);
    const client = Number(digits);
    return /^\d+$/.test(digits) && Number.isSafeInteger(client) ? client : undefined;
  } catch {
    return undefined;
  }
}
