// packages/cipher-client/src/answer-text.ts
//
// The body of an answer a machine identity reads from a service in front of the broker (Google's
// security token service and IAM Credentials, GitHub's OIDC token endpoint), read to a bound.

/** The most of an answer that is read, as Go's `io.LimitReader` bounds it, so a server in the
 *  service's place cannot grow this process without bound. */
export const MAX_ANSWER_BYTES = 1 << 20;

/** The first {@link MAX_ANSWER_BYTES} of `answer`'s body, as text. */
export async function answerText(answer: Response): Promise<string> {
  const reader = answer.body?.getReader();
  if (reader === undefined) return "";
  const chunks: Uint8Array[] = [];
  let read = 0;
  while (read < MAX_ANSWER_BYTES) {
    const { done, value } = await reader.read();
    if (done) break;
    const kept = value.subarray(0, MAX_ANSWER_BYTES - read);
    chunks.push(kept);
    read += kept.length;
  }
  await reader.cancel().catch(() => undefined);
  return Buffer.concat(chunks).toString("utf8");
}
