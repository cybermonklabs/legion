import type { InboundOutageKind } from "@legion/envoy-client/inbound-outage"

/** The stderr line of an outage of one kind, with the failure's message, and what the session is
 *  told of it, as the line of a notice that could not be shown names it. */
interface OutageLines {
  readonly line: (why: string) => string
  readonly told: string
}

/** The stderr line of an outage that leaves the session nothing. */
const RECEIVES_NOTHING: OutageLines = {
  line: (why) =>
    `envoy-channel: this session receives nothing (${why}); registering again every heartbeat\n`,
  told: "that it receives nothing",
}

/**
 * What the server's stderr says of an outage the outage report told (`inboundUnavailable`), by its
 * kind (`InboundOutageKind`, which says what each kind means for what reaches the session). The
 * session's notice card beside it is `inboundUnavailableNoticeText`.
 */
export const INBOUND_UNAVAILABLE_LINES: Record<InboundOutageKind, OutageLines> = {
  unrecorded: {
    line: (why) =>
      `envoy-channel: this session's registration is not recorded for certain (${why}), and the session registers again at every heartbeat\n`,
    told: "that its registration is not recorded for certain",
  },
  github_withheld: {
    line: (why) =>
      `envoy-channel: the listener follows none of this session's GitHub topics now (${why}); the session's other messages still reach it if the listener already held its registration, and none reach it until a registration is accepted if it held none; registering again every heartbeat\n`,
    told: "that its GitHub messages are withheld",
  },
  refused: RECEIVES_NOTHING,
  no_relay: RECEIVES_NOTHING,
}

/** The stderr line of an outage of one kind that ended, and what the session is told of it. */
interface RestoredLines {
  readonly line: string
  readonly told: string
}

/** The stderr line of a session that receives again after it received nothing. */
const RECEIVES_AGAIN: RestoredLines = {
  line: "envoy-channel: this session receives again: a registration was accepted\n",
  told: "that it receives again",
}

/**
 * What the server's stderr says once a registration is accepted after a told outage, by the kind
 * of that outage (`InboundOutageKind`). The session's notice card beside it is
 * `inboundRestoredNoticeText`.
 */
export const INBOUND_RESTORED_LINES: Record<InboundOutageKind, RestoredLines> = {
  unrecorded: {
    line: "envoy-channel: the listener recorded a registration of this session\n",
    told: "that the listener recorded its registration",
  },
  github_withheld: {
    line: "envoy-channel: the listener follows this session's GitHub topics again: a registration was accepted\n",
    told: "that the listener follows its GitHub topics again",
  },
  refused: RECEIVES_AGAIN,
  no_relay: RECEIVES_AGAIN,
}
