package api

// 20261127182557 defect-request-of-a-person: `POST /legion/v1/defects/withdraw` gains one
// refusal, `409 DEFECT_REQUEST_OF_A_PERSON`. A
// request for new work on an issue whose work merged is a reason of the issue's next change, kind
// `request`, and its role says who asked: `person`, `controller` or `architect`. The owning
// architect's withdrawal of a request a person made is refused with this code, and changes
// nothing: such a request ends with no pull request only on that person's word, the person's
// `done` of the issue. The architect asks the person instead. The refusal's shape is the one every
// refusal has, so no answer moves, no request changes, and no state key arrives: a caller below
// this entry is answered the code as any other refusal code, which no plugin enumerates. A route's
// own status write also says who asked for it, which the record writes as the request's role:
// `set_status` with the operator's grant (`legion issue status`) asks as a person, with a
// controller session's grant as the controller, and `rerun_child` and `release_children` as an
// architect. A write through the operator's grant confirms no never-merged work, since under
// `controller: operator` the controller's agent holds the operator token too: only a person's
// `backlog` in Dispatch does. No request or answer of those routes changes.
const defectRequestOfAPersonContract = 20261127182557

func init() {
	registerEntry(contractEntry{id: defectRequestOfAPersonContract, slug: "defect-request-of-a-person"})
}
