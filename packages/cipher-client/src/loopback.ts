// packages/cipher-client/src/loopback.ts
//
// The one rule for a host a plain-http URL may name. It imports nothing, so any module can take
// it without an import cycle.

/** A host a plain-http URL that carries a credential may be on: this machine's loopback
 *  (`localhost`, an address of 127.0.0.0/8, or `[::1]`), as a dev stack's broker and a stand-in
 *  GitHub are. */
export const LOOPBACK_HOST = /^(localhost|127(\.\d{1,3}){3}|\[::1\])$/i;
