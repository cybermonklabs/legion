package api

// 20261126104154 checks: an issue whose plan merges the work of several tasks merges more than once in one run, and
// each merge opens a production check of its own, so an issue can hold several open checks at
// once. Each issue of `GET /legion/v1/state` and of `GET /legion/v1/issues/{key}/record` gains
// `checks`: each open check (`api.CheckView`), oldest first, absent while none is open. `check`
// stays, the newest open check, so a reader of one check reads what it read before. A plugin below
// 267 is served each issue with no `checks` and with its `check` (shaping.go's band,
// `withoutChecks`); a check's own fields and values are those `check` carries, so no value strip
// is owed.
// The same change adds the notice kind `pull-request-close-refused`, to the issue's architect, for
// the pull requests of tasks whose close GitHub refused as it would refuse it again, so each stays
// open (`record.validNoticeKinds`). An outbox payload is decoded strictly, so no daemon of an
// earlier release decodes such a row, as the `task-join-failed` entry above says of its own kind;
// the way back of this change's migration (`store/migrations/20261123221142_task_base.up.sql`)
// deletes those rows before a rollback. No shape of `GET /legion/v1/state` moves with it.
// The entry's allocated number is 267, above main's released contract 256. Earlier released
// records keep their shapes without `checks`; checksContract and SupportedContracts name this
// entry's arrival, not the daemon's next version. Read the base's number and every open pull
// request's declared number again before pushing a renumber.
//
// checksContract is the contract that added `checks` to each issue of the state and of its record
// (version.go's 267): each open production check of the issue, oldest first, since an issue whose
// tasks merged one after another holds a check of each merge. A plugin before it is served every
// issue without the key (withoutChecks) and with `check`, the newest open check, as it read an
// issue of one check before: one built before sjawhar/legion#2788 reads the state with a strict
// schema and would refuse the whole body for a key it does not name, and one built after strips
// the key instead. A check's fields and their values are the ones `check` already carries, so no
// value strip is owed.
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes moves
// that constant, and the contract this key arrived at stays where it is. It moves only with this
// change's own number, and it is the last number of answers.go's SupportedContracts, which names
// the constant rather than its number for that reason.
const checksContract = 20261126104154

func init() {
	registerEntry(contractEntry{
		id: checksContract, slug: "checks",
		bands: []stateNarrowing{
			{contract: checksContract, issue: withoutChecks, dropped: "the issues' checks"},
		},
	})
}
