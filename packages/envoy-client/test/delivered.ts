import type { KeyTakenResult } from "../src/transport";

/**
 * result as a delivered send's or publish's, which a test reads field by field; a key-taken
 * result, which carries none of those fields, fails the test that expected a delivery.
 */
export function delivered<T extends { readonly keyTaken?: undefined }>(
  result: T | KeyTakenResult
): T {
  if (result.keyTaken !== undefined) {
    throw new Error(`expected a delivered result, got key taken by ${result.keyTaken.eventId}`);
  }
  return result;
}
