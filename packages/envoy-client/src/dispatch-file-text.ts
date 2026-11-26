// How the `dispatch` command reads the text of every `--<field>-file <path>` and
// `--<field>-json-file <path>`, and of stdin for `-`: as bytes, decoded as UTF-8 strictly. A lenient
// read writes each byte that is not UTF-8 as U+FFFD, which the call then stores in place of the
// writer's byte; this read throws `NotUtf8Error` instead, which carries the bytes, and the command
// refuses them in Dispatch's own words, its dry run and its call alike (`parseCommand`'s
// `unstorable` parse, which `runDispatchCli` refuses). A byte order mark is kept, as U+FEFF, as the
// lenient read kept it, so the text it decodes gives back, encoded, the bytes it was read from
// (`fileTextBytes`). The `dispatch` entry (`bin/dispatch.ts`) reads through `readFileText`, and
// a test hands it stdin's bytes. Dispatch's refusal of bytes it cannot store (`unstorableBytes`)
// names the first U+0000 or byte that is not UTF-8, whichever comes first, as Dispatch names it.
import { isUtf8 } from "node:buffer";
import { readFileSync } from "node:fs";

const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
const encoder = new TextEncoder();

/** A byte as Dispatch's refusal writes it: `0x` and two upper-case hex digits (`%02X`). */
function byteText(byte: number): string {
  return `0x${byte.toString(16).toUpperCase().padStart(2, "0")}`;
}

/** Bytes a file or stdin held that are not UTF-8, which `readFileText` does not decode: `bytes`
 *  is every byte it read, which the command refuses in Dispatch's own words. */
export class NotUtf8Error extends Error {
  override readonly name = "NotUtf8Error";

  constructor(readonly bytes: Uint8Array) {
    super("the bytes are not UTF-8");
  }
}

/**
 * Where the first byte of `bytes` that is not UTF-8 stands, as Go's `utf8.DecodeRune` finds it,
 * which answers such a byte as a RuneError of width 1, or -1 where every byte is UTF-8. It is a
 * byte no sequence opens with (a continuation byte, 0xC0, 0xC1, 0xF5 to 0xFF), or one that opens a
 * sequence the bytes after it cut short or break: too few of them, or one outside the range its
 * place takes, which also refuses an overlong form, a surrogate and a code point past U+10FFFF.
 */
function firstNotUtf8(bytes: Uint8Array): number {
  for (let at = 0; at < bytes.length; ) {
    const lead = bytes[at] as number;
    if (lead < 0x80) {
      at += 1;
      continue;
    }
    // The width of the sequence `lead` opens, and the range of the byte after it.
    let width: number;
    let low = 0x80;
    let high = 0xbf;
    if (lead >= 0xc2 && lead <= 0xdf) {
      width = 2;
    } else if (lead >= 0xe0 && lead <= 0xef) {
      width = 3;
      if (lead === 0xe0) low = 0xa0;
      if (lead === 0xed) high = 0x9f;
    } else if (lead >= 0xf0 && lead <= 0xf4) {
      width = 4;
      if (lead === 0xf0) low = 0x90;
      if (lead === 0xf4) high = 0x8f;
    } else {
      return at;
    }
    if (at + width > bytes.length) return at;
    const second = bytes[at + 1] as number;
    if (second < low || second > high) return at;
    for (let next = at + 2; next < at + width; next += 1) {
      if (((bytes[next] as number) & 0xc0) !== 0x80) return at;
    }
    at += width;
  }
  return -1;
}

/**
 * The first character of `bytes` Dispatch cannot store, as its `unstorableText`
 * (packages/envoy/internal/dispatch/api/storable_text.go) finds it, reading the bytes in order with
 * Go's `utf8.DecodeRune`: a NUL (U+0000, `byte` 0) or a byte that is not UTF-8 (`firstNotUtf8`),
 * whichever comes first, or undefined for bytes it stores. `character` is where it stands, counted
 * from 1 in UTF-16 units, as Dispatch counts it (`itemblocks.Units16` of the text before it, plus
 * one); a byte order mark before it is one unit, as U+FEFF.
 */
function firstUnstorable(
  bytes: Uint8Array
): { readonly byte: number; readonly character: number } | undefined {
  // `isUtf8` answers natively what `firstNotUtf8(bytes) === -1` answers, so the byte loop runs
  // only for bytes that are not UTF-8.
  const notUtf8 = isUtf8(bytes) ? -1 : firstNotUtf8(bytes);
  const nul = bytes.indexOf(0);
  // A NUL is a whole character of UTF-8. Where it cuts short a sequence that a byte before it
  // opens, that byte comes first, and `firstNotUtf8` names it.
  const at = nul !== -1 && (notUtf8 === -1 || nul < notUtf8) ? nul : notUtf8;
  if (at === -1) return undefined;
  // Every byte before `at` is UTF-8, so the decoder takes them.
  return { byte: bytes[at] as number, character: decoder.decode(bytes.subarray(0, at)).length + 1 };
}

/**
 * Dispatch's refusal of `field` holding `bytes`, or undefined for bytes it stores: the first
 * U+0000 or byte that is not UTF-8 among them (`firstUnstorable`), in the code and words
 * `unstorableText` refuses it with, NUL_CHARACTER or INVALID_UTF8, which `messageFor` prints as it
 * prints that refusal from Dispatch.
 */
export function unstorableBytes(field: string, bytes: Uint8Array): Error | undefined {
  const first = firstUnstorable(bytes);
  if (first === undefined) return undefined;
  const { byte, character } = first;
  return new Error(
    byte === 0
      ? `NUL_CHARACTER: ${field} holds a NUL character (U+0000) at character ${character}, which Dispatch cannot store`
      : `INVALID_UTF8: ${field} holds a byte that is not UTF-8 (${byteText(byte)}) at character ${character}, which Dispatch cannot store`
  );
}

/** `bytes` decoded as UTF-8, a byte order mark kept as U+FEFF; `NotUtf8Error` for bytes that are
 *  not UTF-8. */
function utf8Text(bytes: Uint8Array): string {
  try {
    return decoder.decode(bytes);
  } catch (error) {
    if (isUtf8(bytes)) throw error;
    throw new NotUtf8Error(bytes);
  }
}

/** The text of the file `path` names, or of all of stdin for `-` (`readStdin`: file descriptor
 *  0, unless a test hands its own bytes), read as bytes and decoded by `utf8Text`. */
export function readFileText(
  path: string,
  readStdin: () => Uint8Array = () => readFileSync(0)
): string {
  return utf8Text(path === "-" ? readStdin() : readFileSync(path));
}

/** The bytes `readFileText` decoded `text` from: `text` in UTF-8. Encoding gives back every byte
 *  read, since the strict decoder takes only UTF-8 and keeps a byte order mark as U+FEFF. */
export function fileTextBytes(text: string): Uint8Array {
  return encoder.encode(text);
}
