package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/cipher/wire"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/credential-grants", Auth: authAny, Versions: onlyV2,
		Description: "The live credential grants of the caller's Live grants page, from the secrets broker, newest " +
			"first: those the caller approved, those of the caller's own sessions, those the model judge approved in " +
			"anyone's stead, and those a pod that acts for no person took without asking (a service's pod, or a " +
			"person's pod the broker no longer maps), which nobody may revoke; each {grant_id, granted, record_id, " +
			"enrollment, names, approver, decided_by, expires_at, created_at}. A grant the caller approved, or whose " +
			"session acts for the caller, is listed whatever project it touches; a person's pod the broker no longer " +
			"maps to the caller acts for no one, so its other grants are listed to the caller naming no operator. Any " +
			"other grant of a private project the caller may not see is left out, and one the broker cannot ask " +
			"Dispatch about is left out while it cannot. Dispatch names the caller from its own session, so nobody " +
			"lists another person's. A caller Dispatch calls a restricted member is listed the grants of the " +
			"sessions that act for them alone; while the broker cannot ask Dispatch what kind of member the caller " +
			"is, the list holds no other grant either. Needs the person scope. " + brokerPageText + ". " +
			brokerRelayText + ".",
		Handler:  s.listCredentialGrantsV2,
		Resource: "credential-grants", Kind: kindCollection, Key: "{id}",
		Query:       brokerListV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: page[wire.ApproverGrantResp]{}, Refusals: brokerRefusalAnswers(brokerGrantsCall)},
		OAuthScopes: v2PersonScope,
		Refusals:    listRefusals(brokerRefusals(nil, brokerGrantsCall)...),
	}
})
