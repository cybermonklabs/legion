// packages/envoy/internal/broker/judge/model_token_test.go
// The bearer the judge presents to its model, as NewModel wires it from a
// brokertoken.GoogleAccount: the metadata server's identity token with no account named, the token
// generateIdToken mints with one, and a referral, with no call to the model, when that mint is
// refused. The metadata server and IAM Service Account Credentials are brokertokentest's
// stands-in, and the model is this package's own.
package judge

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
)

// judgeAccount is the service account these judges present the identity token of, a placeholder
// of the form secrets.ValidServiceAccountEmail takes, and gatewayAudience the audience the token
// is minted for, the model gateway's.
const (
	judgeAccount    = "judge-model@example-project.iam.gserviceaccount.com"
	gatewayAudience = "https://gateway.example"
)

// tokenModel is the Model NewModel builds on account, against the model stand-in, which speaks
// Chat Completions and so is presented the token as Authorization: Bearer.
func tokenModel(model *modelStandIn, account *brokertoken.GoogleAccount) Model {
	return NewModel(model.server.URL, testModel, string(agentloop.OpenAIChat), account)
}

// tokenData is the block these judges ask the model about. What it says does not matter here.
func tokenData() Data {
	return Data{
		Secret: testSecret,
		Reason: pending().Reason,
		Issue:  IssueData{Key: testIssue, Title: "The broker's model judge", Status: "doing"},
	}
}

// TestTheJudgePresentsTheMetadataIdentityTokenWithNoAccount: with BROKER_JUDGE_MODEL_ACCOUNT
// unset the judge presents the metadata server's identity token, as it always has, and asks IAM
// Service Account Credentials nothing, though one is named for it to reach.
func TestTheJudgePresentsTheMetadataIdentityTokenWithNoAccount(t *testing.T) {
	brokertokentest.NewMetadataStand(t)
	iam := brokertokentest.NewIAMStand(t)
	model := newModel(t, verdictReply(Approve, "The reason names the issue."))
	account := brokertoken.NewGoogleAccount(gatewayAudience)
	account.IAMCredentialsURL = iam.Server.URL

	verdict, _, _, err := tokenModel(model, account).Ask(t.Context(), tokenData())
	if err != nil || verdict.Word != Approve {
		t.Fatalf("Ask = %+v, %v; want the model's approval", verdict, err)
	}
	if got, want := model.presented(), "Bearer "+brokertokentest.MetadataIdentityToken; len(got) != 1 || got[0] != want {
		t.Errorf("the model was presented %q; want one call with %q, the metadata server's identity token", got, want)
	}
	if got := iam.Requests(); len(got) != 0 {
		t.Errorf("IAM Service Account Credentials got %v; want no request from a judge with no account", got)
	}
}

// TestTheJudgePresentsTheMintedTokenWithAnAccount: with an account named the judge presents the
// token generateIdToken mints for it, from one call of the shape Google's reference gives,
// carrying the pod's own access token, and never asks the metadata server's identity path.
func TestTheJudgePresentsTheMintedTokenWithAnAccount(t *testing.T) {
	metadata := brokertokentest.NewMetadataStand(t)
	iam := brokertokentest.NewIAMStand(t)
	model := newModel(t, verdictReply(Approve, "The reason names the issue."))
	account := brokertoken.NewGoogleAccount(gatewayAudience)
	account.ServiceAccount = judgeAccount
	account.IAMCredentialsURL = iam.Server.URL

	verdict, _, _, err := tokenModel(model, account).Ask(t.Context(), tokenData())
	if err != nil || verdict.Word != Approve {
		t.Fatalf("Ask = %+v, %v; want the model's approval", verdict, err)
	}
	if got, want := model.presented(), "Bearer "+brokertokentest.IAMIdentityToken; len(got) != 1 || got[0] != want {
		t.Errorf("the model was presented %q; want one call with %q, the minted token", got, want)
	}
	requests := iam.Requests()
	if len(requests) != 1 {
		t.Fatalf("IAM Service Account Credentials got %d requests; want the one mint", len(requests))
	}
	mint := requests[0]
	if want := "/v1/projects/-/serviceAccounts/" + judgeAccount + ":generateIdToken"; mint.Method != http.MethodPost || mint.Path != want {
		t.Errorf("the mint was %s %s; want POST %s", mint.Method, mint.Path, want)
	}
	if mint.Body["includeEmail"] != true || mint.Body["audience"] != gatewayAudience {
		t.Errorf("the mint asked %v; want includeEmail true and the audience %s", mint.Body, gatewayAudience)
	}
	if want := "Bearer " + brokertokentest.MetadataAccessBearer; mint.Authorization != want {
		t.Errorf("the mint carried Authorization %q; want %q, the pod's own access token", mint.Authorization, want)
	}
	if got := metadata.Asked(brokertokentest.MetadataIdentityPath); got != 0 {
		t.Errorf("the metadata server's identity path was asked %d times; want none with an account named", got)
	}
}

// TestTheJudgeRefersWhenTheMintIsRefused: a mint Google refuses leaves the judge no token, so it
// refers the request, with an error naming the account and the audience for its audit row, and
// never calls the model, with the pod's own identity token or with none. The metadata server here
// would still hand out an identity token, so a judge that fell back would have reached the model.
func TestTheJudgeRefersWhenTheMintIsRefused(t *testing.T) {
	metadata := brokertokentest.NewMetadataStand(t)
	iam := brokertokentest.NewIAMStand(t)
	iam.AnswersStatus(http.StatusForbidden, `{"error":{"code":403,"message":"Permission 'iam.serviceAccounts.getOpenIdToken' denied on resource (or it may not exist).","status":"PERMISSION_DENIED"}}`)
	model := newModel(t, verdictReply(Approve, "The reason names the issue."))
	account := brokertoken.NewGoogleAccount(gatewayAudience)
	account.ServiceAccount = judgeAccount
	account.IAMCredentialsURL = iam.Server.URL

	verdict, _, _, err := tokenModel(model, account).Ask(t.Context(), tokenData())
	if verdict.Word != Refer || err == nil {
		t.Fatalf("Ask = %+v, %v; want a referral with an error", verdict, err)
	}
	for _, want := range []string{judgeAccount, gatewayAudience, "HTTP 403"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the referral's error %q does not name %q", err, want)
		}
	}
	if got := model.asked(); got != 0 {
		t.Errorf("the model got %d calls; want none without a token", got)
	}
	if got := metadata.Asked(brokertokentest.MetadataIdentityPath); got != 0 {
		t.Errorf("the metadata server's identity path was asked %d times; a refused mint falls back to nothing", got)
	}
}
