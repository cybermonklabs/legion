package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The box print test of this command's holders: each holds the probe in every box field, and its
// twins have the same shape with the probe as a string there (redacttest.PrintTest).

type apiErrorTwin struct {
	Status   int
	code     string
	said     string
	answerID string
	withheld bool
}

func (e *apiErrorTwin) Error() string {
	return fmt.Sprintf("%s (%s) (answer %s)", e.said, e.code, e.answerID)
}

type apiErrorExported struct {
	Status   int
	Code     string
	Said     string
	AnswerID string
	Withheld bool
}

// The twins of the decoded answers: each answer's shape with a string where it holds the box.

type requestResultTwin struct {
	RequestID       string           `json:"request_id"`
	State           string           `json:"state"`
	Secrets         []SecretDecision `json:"secrets"`
	GrantID         *string          `json:"grant_id"`
	RecordID        *string          `json:"record_id"`
	Approver        *string          `json:"approver"`
	LifetimeSeconds *int             `json:"lifetime_seconds"`
	Coalesced       bool             `json:"coalesced,omitempty"`
	Detail          *string          `json:"detail"`
	ApproveURL      string           `json:"approve_url"`
	JudgeDeciding   bool             `json:"judge_deciding"`
}

type requestDecisionTwin struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

type requestStatusTwin struct {
	State         string               `json:"state"`
	GrantID       *string              `json:"grant_id"`
	RecordID      *string              `json:"record_id"`
	Approver      *string              `json:"approver"`
	DecidedAt     *time.Time           `json:"decided_at"`
	Decision      *requestDecisionTwin `json:"decision"`
	Detail        *string              `json:"detail"`
	ApproveURL    string               `json:"approve_url"`
	JudgeDeciding bool                 `json:"judge_deciding"`
}

type grantTwin struct {
	GrantID   string     `json:"grant_id"`
	RequestID string     `json:"request_id"`
	Approver  *string    `json:"approver"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type selfResultTwin struct {
	EnrollmentID   string      `json:"enrollment_id"`
	Kind           string      `json:"kind"`
	Operator       *string     `json:"operator"`
	LeaseExpiresAt time.Time   `json:"lease_expires_at"`
	Grants         []grantTwin `json:"grants"`
}

type operatorMachineTwin struct {
	CredentialID string    `json:"credential_id"`
	Host         string    `json:"host"`
	Service      *string   `json:"service"`
	ApprovedBy   string    `json:"approved_by"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Expired      bool      `json:"expired"`
}

type operatorMachinesTwin struct {
	Credentials []operatorMachineTwin `json:"credentials"`
}

type operatorGrantEnrollmentTwin struct {
	Kind      string  `json:"kind"`
	RuntimeID string  `json:"runtime_id"`
	Operator  string  `json:"operator"`
	Slot      *string `json:"slot"`
}

type operatorGrantTwin struct {
	GrantID    string                      `json:"grant_id"`
	Granted    string                      `json:"granted"`
	RecordID   *string                     `json:"record_id"`
	Enrollment operatorGrantEnrollmentTwin `json:"enrollment"`
	Names      []string                    `json:"names"`
	Approver   *string                     `json:"approver"`
	DecidedBy  *string                     `json:"decided_by"`
	ExpiresAt  *time.Time                  `json:"expires_at"`
	CreatedAt  time.Time                   `json:"created_at"`
}

type operatorGrantsTwin struct {
	Grants []operatorGrantTwin `json:"grants"`
}

type appliedNarrowingTwin struct {
	Owner        string   `json:"owner"`
	Repositories []string `json:"repositories,omitempty"`
	ReadOnly     bool     `json:"read_only,omitempty"`
}

type rereadTwin struct {
	Name   string `json:"name"`
	Served bool   `json:"served"`
	Reason string `json:"reason"`
	Type   string `json:"type"`
	Told   bool   `json:"-"`
}

func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	code, words := redact.NewRemoteText(redacttest.Code), redact.NewRemoteText(redacttest.Words)
	answerID := redact.NewRemoteText(redacttest.FragmentWords)
	type doStatusTwin = struct {
		Code     string `json:"code"`
		Error    string `json:"error"`
		AnswerID string `json:"answer_id"`
	}
	type enrollmentTwin = struct {
		EnrollmentID string `json:"enrollment_id"`
	}
	type credentialTwin = struct {
		CredentialID string `json:"credential_id"`
	}
	type userinfoTwin = struct {
		Email string `json:"email"`
	}
	probe := redacttest.Words
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	decision := requestDecision{By: words, At: at}
	decisionTwin := requestDecisionTwin{By: probe, At: at}
	grant := Grant{GrantID: words, RequestID: words, ExpiresAt: &at}
	grantTwinValue := grantTwin{GrantID: probe, RequestID: probe, ExpiresAt: &at}
	machine := OperatorMachine{CredentialID: words, Host: words, ApprovedBy: words, IssuedAt: at, ExpiresAt: at}
	machineTwin := operatorMachineTwin{CredentialID: probe, Host: probe, ApprovedBy: probe, IssuedAt: at, ExpiresAt: at}
	enrollment := OperatorGrantEnrollment{Kind: words, RuntimeID: words, Operator: words, Slot: &words}
	enrollmentTwinValue := operatorGrantEnrollmentTwin{Kind: probe, RuntimeID: probe, Operator: probe, Slot: &probe}
	operatorGrant := OperatorGrant{GrantID: words, Granted: words, Enrollment: enrollment, Names: []redact.RemoteText{words},
		Approver: &words, DecidedBy: &words, ExpiresAt: &at, CreatedAt: at}
	operatorGrantTwinValue := operatorGrantTwin{GrantID: probe, Granted: probe, Enrollment: enrollmentTwinValue, Names: []string{probe},
		Approver: &probe, DecidedBy: &probe, ExpiresAt: &at, CreatedAt: at}
	applied := AppliedNarrowing{Owner: words, Repositories: []redact.RemoteText{words}}
	appliedTwin := appliedNarrowingTwin{Owner: probe, Repositories: []string{probe}}
	for _, h := range []redacttest.Holder{
		{
			Name:     "agent-secrets.apiError",
			Kind:     "error",
			Boxed:    newAPIError(403, code, words, answerID, false),
			Same:     &apiErrorTwin{403, redacttest.Code, redacttest.Words, redacttest.FragmentWords, false},
			Exported: &apiErrorExported{403, redacttest.Code, redacttest.Words, redacttest.FragmentWords, false},
		},
		{
			Name: "agent-secrets.client.doStatus.envelope",
			Kind: "fields",
			Boxed: struct {
				Code     redact.RemoteText `json:"code"`
				Error    redact.RemoteText `json:"error"`
				AnswerID redact.RemoteText `json:"answer_id"`
			}{code, words, answerID},
			Same:     doStatusTwin{redacttest.Code, redacttest.Words, redacttest.FragmentWords},
			Exported: doStatusTwin{redacttest.Code, redacttest.Words, redacttest.FragmentWords},
		},
		{
			Name:     "agent-secrets.RequestResult",
			Kind:     "fields",
			Boxed:    RequestResult{RequestID: words, State: words, GrantID: &words, Approver: &words, Detail: &words, ApproveURL: words},
			Same:     requestResultTwin{RequestID: probe, State: probe, GrantID: &probe, Approver: &probe, Detail: &probe, ApproveURL: probe},
			Exported: requestResultTwin{RequestID: probe, State: probe, GrantID: &probe, Approver: &probe, Detail: &probe, ApproveURL: probe},
		},
		{Name: "agent-secrets.requestDecision", Kind: "fields", Boxed: decision, Same: decisionTwin, Exported: decisionTwin},
		{
			Name:  "agent-secrets.RequestStatus",
			Kind:  "fields",
			Boxed: RequestStatus{State: words, GrantID: &words, Approver: &words, Decision: &decision, Detail: &words, ApproveURL: words},
			Same: requestStatusTwin{State: probe, GrantID: &probe, Approver: &probe, Decision: &decisionTwin, Detail: &probe,
				ApproveURL: probe},
			Exported: requestStatusTwin{State: probe, GrantID: &probe, Approver: &probe, Decision: &decisionTwin, Detail: &probe,
				ApproveURL: probe},
		},
		{Name: "agent-secrets.Grant", Kind: "fields", Boxed: grant, Same: grantTwinValue, Exported: grantTwinValue},
		{
			Name:     "agent-secrets.SelfResult",
			Kind:     "fields",
			Boxed:    SelfResult{EnrollmentID: words, Kind: words, Operator: &words, LeaseExpiresAt: at, Grants: []Grant{grant}},
			Same:     selfResultTwin{EnrollmentID: probe, Kind: probe, Operator: &probe, LeaseExpiresAt: at, Grants: []grantTwin{grantTwinValue}},
			Exported: selfResultTwin{EnrollmentID: probe, Kind: probe, Operator: &probe, LeaseExpiresAt: at, Grants: []grantTwin{grantTwinValue}},
		},
		{Name: "agent-secrets.OperatorMachine", Kind: "fields", Boxed: machine, Same: machineTwin, Exported: machineTwin},
		{
			Name:     "agent-secrets.OperatorMachines",
			Kind:     "fields",
			Boxed:    OperatorMachines{Credentials: []OperatorMachine{machine}},
			Same:     operatorMachinesTwin{Credentials: []operatorMachineTwin{machineTwin}},
			Exported: operatorMachinesTwin{Credentials: []operatorMachineTwin{machineTwin}},
		},
		{Name: "agent-secrets.OperatorGrantEnrollment", Kind: "fields", Boxed: enrollment, Same: enrollmentTwinValue, Exported: enrollmentTwinValue},
		{Name: "agent-secrets.OperatorGrant", Kind: "fields", Boxed: operatorGrant, Same: operatorGrantTwinValue, Exported: operatorGrantTwinValue},
		{
			Name:     "agent-secrets.OperatorGrants",
			Kind:     "fields",
			Boxed:    OperatorGrants{Grants: []OperatorGrant{operatorGrant}},
			Same:     operatorGrantsTwin{Grants: []operatorGrantTwin{operatorGrantTwinValue}},
			Exported: operatorGrantsTwin{Grants: []operatorGrantTwin{operatorGrantTwinValue}},
		},
		{Name: "agent-secrets.AppliedNarrowing", Kind: "fields", Boxed: applied, Same: appliedTwin, Exported: appliedTwin},
		// GrantValues holds the box only behind its GitHubApp pointer, which fmt prints as an
		// address: it has no same-shape twin a printer reaches, so its rows use the kind's own.
		{Name: "agent-secrets.GrantValues", Kind: "fields", Boxed: GrantValues{GitHubApp: &applied}},
		{
			Name:     "agent-secrets.Reread",
			Kind:     "fields",
			Boxed:    Reread{Name: "HELD_KEY", Reason: words, Type: words, Told: true},
			Same:     rereadTwin{Name: "HELD_KEY", Reason: probe, Type: probe, Told: true},
			Exported: rereadTwin{Name: "HELD_KEY", Reason: probe, Type: probe, Told: true},
		},
		{Name: "agent-secrets.googleUserinfo", Kind: "fields", Boxed: googleUserinfo{Email: words}, Same: userinfoTwin{probe}, Exported: userinfoTwin{probe}},
		{
			Name: "agent-secrets.enrollUnderCredential.answer",
			Kind: "fields",
			Boxed: struct {
				EnrollmentID redact.RemoteText `json:"enrollment_id"`
			}{words},
			Same:     enrollmentTwin{probe},
			Exported: enrollmentTwin{probe},
		},
		{
			Name: "agent-secrets.client.CreatePodEnrollment.answer",
			Kind: "fields",
			Boxed: struct {
				EnrollmentID redact.RemoteText `json:"enrollment_id"`
			}{words},
			Same:     enrollmentTwin{probe},
			Exported: enrollmentTwin{probe},
		},
		{
			Name: "agent-secrets.client.TokenLogin.answer",
			Kind: "fields",
			Boxed: struct {
				CredentialID redact.RemoteText `json:"credential_id"`
			}{words},
			Same:     credentialTwin{probe},
			Exported: credentialTwin{probe},
		},
	} {
		t.Run(h.Name, func(t *testing.T) { redacttest.PrintTest(t, h) })
	}
}
