import type { InboundOutageKind } from "@legion/envoy-client/inbound-outage";

/** The log line and the pane note of an outage of one kind, each with the failure's message. */
interface OutageLines {
  readonly log: (why: string) => string;
  readonly pane: (why: string) => string;
}

/** The log line and the pane note of an outage that leaves the session nothing. */
const RECEIVES_NOTHING: OutageLines = {
  log: (why) => `envoy: this session receives nothing: ${why}`,
  pane: (why) =>
    `envoy: inbound messages are unavailable (${why}); registering again every heartbeat`,
};

/**
 * What the extension's log (at warn) and the session's pane say of an outage the outage report
 * told (`inboundUnavailable`), by its kind (`InboundOutageKind`, which says what each kind means
 * for what reaches the session). The session's notice beside them is
 * `inboundUnavailableNoticeText`.
 */
export const INBOUND_UNAVAILABLE_LINES: Record<InboundOutageKind, OutageLines> = {
  unrecorded: {
    log: (why) => `envoy: this session's registration is not recorded for certain: ${why}`,
    pane: (why) =>
      `envoy: this session's registration is not recorded for certain (${why}), and the session registers again at every heartbeat`,
  },
  github_withheld: {
    log: (why) =>
      `envoy: the listener follows none of this session's GitHub topics now; the session's other messages still reach it if the listener already held its registration, and none reach it until a registration is accepted if it held none: ${why}`,
    pane: (why) =>
      `envoy: GitHub messages are withheld (${why}); other messages still arrive if the listener already held this session's registration, and none arrive until a registration is accepted if it held none; registering again every heartbeat`,
  },
  refused: RECEIVES_NOTHING,
  no_relay: RECEIVES_NOTHING,
};

/** The log line and the pane note of an outage of one kind that ended. */
interface RestoredLines {
  readonly log: string;
  readonly pane: string;
}

/** The log line and the pane note of a session that receives again after it received nothing. */
const RECEIVES_AGAIN: RestoredLines = {
  log: "envoy: this session receives again: a registration was accepted",
  pane: "envoy: inbound messages are back",
};

/**
 * What the extension's log (at info) and the session's pane say once a registration is accepted
 * after a told outage, by the kind of that outage (`InboundOutageKind`). The session's notice
 * beside them is `inboundRestoredNoticeText`.
 */
export const INBOUND_RESTORED_LINES: Record<InboundOutageKind, RestoredLines> = {
  unrecorded: {
    log: "envoy: the listener recorded a registration of this session",
    pane: "envoy: the listener recorded a registration of this session",
  },
  github_withheld: {
    log: "envoy: the listener follows this session's GitHub topics again: a registration was accepted",
    pane: "envoy: the listener follows this session's GitHub topics again",
  },
  refused: RECEIVES_AGAIN,
  no_relay: RECEIVES_AGAIN,
};
