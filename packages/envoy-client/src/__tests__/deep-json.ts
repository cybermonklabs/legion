/** An object chain 100,000 levels deep as JSON text, past the recursion of the card's TOON
 *  encoder and of JSON.stringify alike. The encoder reads the whole value before it lays out a
 *  line, with a recursion of its own, and under `bun test` (Bun 1.4.4) that read overflows the
 *  stack somewhere past 20,000 levels, by what ran before it; at 20,000 the card's bound was
 *  sometimes reached first. 100,000 levels is an 800 KB frame, inside the hub's limit. Every row
 *  that needs a value past the encoder takes this one, so a Bun that moves the limit moves one
 *  depth. */
export const objectChainPastTheEncoder = `${'{"a":'.repeat(100_000)}1${"}".repeat(100_000)}`;
