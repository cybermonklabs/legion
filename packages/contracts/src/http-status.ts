// The rules this workspace's TypeScript reads another service's HTTP status by: `outage`, whether
// the same request may be answered if it is asked again, whose Go half is `httpoutage.Is`
// (packages/envoy/httpoutage), the same set; `mayHaveReachedServer`, whether a failed request may
// have reached the server and taken effect; and `retryAfterMs`, the wait an answer's
// `Retry-After` asks for. This file names none of its readers, so no list of them goes stale: a
// search for imports of `http-status` finds them. It imports nothing, so any module of the
// contract, and a script outside any workspace, can read it.

/**
 * Whether `status` is a server that could not answer the request now, so the same request may be
 * answered if it is asked again: any 5xx (RFC 9110 section 15.6, the server failing to answer), a
 * 408 Request Timeout (section 15.5.9: the server did not receive the whole request in time, and
 * the client may repeat it) and a 429 Too Many Requests (RFC 6585 section 4: try later). Every
 * other status is the server's answer, which asking again repeats. A reader whose own contract
 * says otherwise for one of them says so where it reads it.
 */
export function outage(status: number): boolean {
  return status >= 500 || status === 408 || status === 429;
}

/**
 * Whether a request answered with `status` may have reached the server and taken effect there, so
 * a write it carried may have been applied. After a 5xx or a 408 it may have:
 * - a hop in front of the server (a proxy or gateway that answers in its place) sends a 5xx when
 *   the server failed, timed out or dropped the connection, possibly once it had the request, and
 *   the server's own 5xx can come after it committed;
 * - a 408 is a timeout that ran out, which some hops count after they forwarded the whole request:
 *   an Envoy proxy answers 408 when its `stream_idle_timeout` fires before any response header
 *   came back, and Google's external Application Load Balancer logs `client_timed_out` with a 408
 *   while it was proxying the request or the response.
 *
 * After a 429 (a rate limit, which turns the request away before it runs) or any other status of a
 * failed request (a refusal) it did not. So it holds of exactly the 5xx and the 408, every one of
 * them an outage (`outage`), and the 429 is the one outage after which the request took no effect.
 */
export function mayHaveReachedServer(status: number): boolean {
  return status >= 500 || status === 408;
}

/**
 * The wait an answer's `Retry-After` header asks for, in milliseconds (RFC 9110 section 10.2.3):
 * its delay-seconds, or the time left until its HTTP-date as `now` reads it (none for a date that
 * has passed); 0 where it sends none or a value that is neither. A reader bounds the wait itself.
 */
export function retryAfterMs(headers: Headers, now: number = Date.now()): number {
  const value = headers.get("Retry-After")?.trim() ?? "";
  if (/^\d+$/.test(value)) return Number(value) * 1_000;
  const at = Date.parse(value);
  return Number.isNaN(at) ? 0 : Math.max(0, at - now);
}
