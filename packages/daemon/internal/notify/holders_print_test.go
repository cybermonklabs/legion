package notify

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/listenerwire"
	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// The box print test of this package's holders (oauth2calltest.PrintTest): each holds the probe in
// every box field, and its twins have the same shape with the probe as a string there.
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	type notDeliveredTwin = struct {
		Reason  string
		Holder  string
		EventID string
	}
	type refusedTwin = struct{ Holder string }
	type deniedTwin = struct{ Message string }
	type deletedTwin = struct {
		Deleted bool
		Reason  string
	}
	type registrationTwin = struct {
		Registered       bool
		ClientInstanceID string
		LastSeen         int64
	}
	type conditionsTwin = struct {
		ClientInstanceID   string
		NotRegisteredAfter time.Time
	}
	kept := false
	at := time.Unix(1762560000, 0).UTC()
	said := oauth2calltest.Said(oauth2calltest.Words)
	// The listener's answers this package and the controller's probe decode: listenerwire's forms
	// with their words held as oauth2call.RemoteText, each beside its twin, the same form with each
	// of those words a string, as the listener writes it.
	words, lastSeen := oauth2calltest.Words, int64(1762560000000)
	registration := listenerwire.SessionRegistrationAs[oauth2call.RemoteText]{
		SessionID: said, State: StateLive, LastSeen: lastSeen, UpdatedAt: lastSeen, ClientInstanceID: said,
	}
	registrationWritten := listenerwire.SessionRegistration{
		SessionID: words, State: StateLive, LastSeen: lastSeen, UpdatedAt: lastSeen, ClientInstanceID: words,
	}
	deleted := listenerwire.DeleteSessionResponseAs[oauth2call.RemoteText]{Deleted: &kept, Reason: said}
	deletedWritten := listenerwire.DeleteSessionResponse{Deleted: &kept, Reason: words}
	holder := listenerwire.RoleHolderAs[oauth2call.RemoteText, oauth2call.RemoteText]{
		Capabilities: []string{}, Holder: said, HolderState: said, LastSeen: &lastSeen, Role: "legion-planner", Title: said,
	}
	holderWritten := listenerwire.RoleHolderAs[string, string]{
		Capabilities: []string{}, Holder: words, HolderState: words, LastSeen: &lastSeen, Role: "legion-planner", Title: words,
	}
	lapsed := listenerwire.RoleRefusalAs[oauth2call.RemoteText, oauth2call.RemoteText, oauth2call.RemoteText]{
		Error: said, Reason: said, Holder: said, HolderState: said, ClientInstanceID: said, LastSeen: &lastSeen,
	}
	lapsedWritten := listenerwire.RoleRefusalAs[string, string, string]{
		Error: words, Reason: words, Holder: words, HolderState: words, ClientInstanceID: words, LastSeen: &lastSeen,
	}
	failure := listenerwire.RoleSendFailureAs[oauth2call.RemoteText]{
		Error: said, Reason: said, EventID: said, DedupeKey: said, Holder: said, Attempts: 2,
	}
	failureWritten := listenerwire.RoleSendFailure{
		Error: words, Reason: words, EventID: words, DedupeKey: words, Holder: words, Attempts: 2,
	}
	capability := listenerwire.CapabilityRefusalAs[oauth2call.RemoteText]{Error: said, Reason: said, Holder: said}
	capabilityWritten := listenerwire.CapabilityRefusal{Error: words, Reason: words, Holder: words}
	denied := listenerwire.APIErrorAs[oauth2call.RemoteText]{Code: said, Error: said, Expected: []oauth2call.RemoteText{said}}
	deniedWritten := listenerwire.APIError{Code: words, Error: words, Expected: []string{words}}
	for _, h := range []oauth2calltest.Holder{
		{
			Name:     "notify.ErrNotDelivered",
			Kind:     "fields",
			Boxed:    ErrNotDelivered{Reason: ReasonReceiptTimeout, Holder: said, EventID: said},
			Same:     notDeliveredTwin{Reason: ReasonReceiptTimeout, Holder: oauth2calltest.Words, EventID: oauth2calltest.Words},
			Exported: notDeliveredTwin{Reason: ReasonReceiptTimeout, Holder: oauth2calltest.Words, EventID: oauth2calltest.Words},
		},
		{
			Name:     "notify.ErrRefusedByHolder",
			Kind:     "fields",
			Boxed:    ErrRefusedByHolder{Holder: said},
			Same:     refusedTwin{Holder: oauth2calltest.Words},
			Exported: refusedTwin{Holder: oauth2calltest.Words},
		},
		{
			Name:     "notify.ErrPublishDenied",
			Kind:     "fields",
			Boxed:    ErrPublishDenied{Message: said},
			Same:     deniedTwin{Message: oauth2calltest.Words},
			Exported: deniedTwin{Message: oauth2calltest.Words},
		},
		{
			Name:     "notify.SessionDeleted",
			Kind:     "fields",
			Boxed:    SessionDeleted{Reason: said},
			Same:     deletedTwin{Reason: oauth2calltest.Words},
			Exported: deletedTwin{Reason: oauth2calltest.Words},
		},
		{
			Name:     "notify.SessionRegistration",
			Kind:     "fields",
			Boxed:    SessionRegistration{Registered: true, ClientInstanceID: said, LastSeen: 1762560000000},
			Same:     registrationTwin{Registered: true, ClientInstanceID: oauth2calltest.Words, LastSeen: 1762560000000},
			Exported: registrationTwin{Registered: true, ClientInstanceID: oauth2calltest.Words, LastSeen: 1762560000000},
		},
		{
			Name:     "notify.DeleteConditions",
			Kind:     "fields",
			Boxed:    DeleteConditions{ClientInstanceID: said, NotRegisteredAfter: at},
			Same:     conditionsTwin{ClientInstanceID: oauth2calltest.Words, NotRegisteredAfter: at},
			Exported: conditionsTwin{ClientInstanceID: oauth2calltest.Words, NotRegisteredAfter: at},
		},
		{
			Name:     "notify.listenerwire.SessionRegistrationAs",
			Kind:     "fields",
			Boxed:    registration,
			Same:     registrationWritten,
			Exported: registrationWritten,
		},
		{
			Name:     "notify.listenerwire.DeleteSessionResponseAs",
			Kind:     "fields",
			Boxed:    deleted,
			Same:     deletedWritten,
			Exported: deletedWritten,
		},
		{
			Name:     "notify.listenerwire.RoleHolderAs",
			Kind:     "fields",
			Boxed:    holder,
			Same:     holderWritten,
			Exported: holderWritten,
		},
		{
			Name:     "notify.listenerwire.RoleRefusalAs",
			Kind:     "fields",
			Boxed:    lapsed,
			Same:     lapsedWritten,
			Exported: lapsedWritten,
		},
		{
			Name:     "notify.listenerwire.RoleSendFailureAs",
			Kind:     "fields",
			Boxed:    failure,
			Same:     failureWritten,
			Exported: failureWritten,
		},
		{
			Name:     "notify.listenerwire.CapabilityRefusalAs",
			Kind:     "fields",
			Boxed:    capability,
			Same:     capabilityWritten,
			Exported: capabilityWritten,
		},
		{
			Name:     "notify.listenerwire.APIErrorAs",
			Kind:     "fields",
			Boxed:    denied,
			Same:     deniedWritten,
			Exported: deniedWritten,
		},
	} {
		t.Run(h.Name, func(t *testing.T) { oauth2calltest.PrintTest(t, h) })
	}
}
