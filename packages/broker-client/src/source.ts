// packages/broker-client/src/source.ts

/** One platform identity read at the moment it is traded: the subject token of the broker's RFC
 *  8693 exchange, and the type the exchange reads it under. */
export interface SubjectToken {
  readonly token: string;
  readonly tokenType: string;
}

/**
 * A platform identity this process can prove, read fresh each time it is asked. The grant issues
 * no refresh token, so every renewal asks its source again — a projected token the kubelet
 * rewrote in place, a metadata token minted anew, a person's identity token renewed at their
 * issuer — and a source that caches must cache for less than the token it answers lives.
 *
 * It mirrors the Go client's `Source` (`packages/envoy/internal/brokerclient/source.go`). The
 * implementations here are a machine's three identities — a pod's projected token
 * (`projected-token.ts`), a Compute Engine machine's own Google identity token
 * (`metadata-identity.ts`) and a Google identity token minted through a workload identity pool
 * (`pool-identity.ts`) — and a person's sign-in on the machine (`person-identity.ts`).
 */
export interface TokenSource {
  /**
   * The subject token to present and the type it is presented under, or a rejected promise that
   * never reached the broker: an identity this machine could not read is never an empty token for
   * the broker to refuse.
   */
  read(signal?: AbortSignal): Promise<SubjectToken>;
  /** Names the identity in a refusal, without naming the token: the file read, the metadata
   *  server asked, the pool federated at, or the person whose sign-in it is. */
  describe(): string;
}
