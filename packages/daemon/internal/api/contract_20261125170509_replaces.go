package api

// 20261125170509 replaces: a claim's registration names the session its new session replaces, so
// the Envoy messages the old session did not read reach the new one. Both answers of `POST
// /legion/v1/claims/register`, a role's (`claim.RegisterResponse`) and the controller's
// (`api.ControllerRegisterResponse`), gain `replaces`, a string absent when empty: the session the
// claim abandoned when it last forgot one (`supervise.Claim.Abandoned`), where that is not the
// session the registration records and no operator's release has freed its names (after a
// release another claim may register that session, and its messages are then that claim's;
// api's replacedSession). The record stays until the claim's next forget, so each later
// registration of the claim names the same session again. The session names it as
// `previous_session_id` in its Envoy role claim, soft or hard, and the Envoy listener hands the
// claimant that session's unread direct and role messages. The operator's controller
// (`controller: operator`) registers on no claim and is answered no `replaces`. The records from
// this entry on hold `replaces` on both answers, and no record before it does: a caller whose
// contract (the one it states, else its pod's highest) is below this entry's number is served
// both answers without it (the two registration rows of shaping.go's shapeAnswer, at
// `entryNumber(replacesContract)`), and placedByPod places the caller of a registration that
// carries it by its pod. The two answers were identity answers of the numbered entries until this
// entry, and shapeAnswer names them now (frozenIdentityAnswers holds neither).
// `packages/contracts/src/legion-api.ts` mirrors the key as optional on both answers. No value of
// an enumerated field moves, and no request a plugin sends to the daemon changes.
//
// A plugin before this entry is served both answers without the key: its reader did not ask for
// the session, and a strict reader refuses a key it does not know. The entry removes no key,
// adds no value to an enumeration, changes no request and adds no row to a table.
const replacesContract = 20261125170509

func init() {
	registerEntry(contractEntry{id: replacesContract, slug: "replaces"})
}
