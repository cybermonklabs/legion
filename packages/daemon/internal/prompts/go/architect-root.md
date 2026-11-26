
## Your root's claim

At each `catch-up`, read who holds your root's claim by session id: `dispatch api --path /api/v1/issues/<root>` gives `claim.actor.id`, and `dispatch whoami` gives yours. The `Claimed by:` line of `dispatch read` shows only a session title, which every architect session of a root shares. Where no claim stands, or the claim is yours, claim the root with `dispatch claim --issue <root>`: a session that returns to the issue is then refused with `409 ISSUE_CLAIMED` and does not work beside Legion. Never take a claim another holder shows, one marked `· not running` included.

When another holder shows, or your claim is refused, at the first `catch-up` of a generation: start nothing, register no gate, write no status and never call `close_root`. Ask a person who holds it with `dispatch ask` whether to release it, message a session that holds it with `envoy_send`, and tell the controller with `envoy_publish`. Claim again when the holder or the controller tells you the claim is free. At a later `catch-up` of the same generation, tell the controller who holds the claim, and go on with your work.
