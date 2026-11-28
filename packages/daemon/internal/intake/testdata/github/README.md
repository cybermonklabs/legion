# Captured GitHub envelopes

Each file is the Envoy envelope the listener publishes for one GitHub webhook delivery, and `intake_test.go` decodes it as the daemon's consumer does.

## `pr-enqueued.json`

GitHub sends no sample of a `pull_request` delivery with the action `enqueued` that a person can fetch again, so this envelope is made by the listener's own normalizer, `contracts.GithubEnvelopes` (`packages/envoy/internal/contracts/normalize.go`), run over GitHub's documented `pull_request` webhook payload for the action `enqueued` on 2026-11-26.
The payload carried the documented members the normalizer reads: `action`, `number`, `pull_request.{number, title, html_url, body, merged, updated_at, user.login, head.{ref, sha, repo.full_name}, base.ref}`, `repository.full_name` and `sender.login`.
The names are the ones of the other envelopes here (`sjawhar/legion#42` on `legion/LEGION-208`, head `head-captured`), so the decoder's tests read one pull request across its actions.

The envelope's `issued_at` is the listener's clock when it published, never the time of the move in the queue.
That time is GitHub's own and comes from the pull request's timeline (`internal/daemon/testdata/queue/`).
