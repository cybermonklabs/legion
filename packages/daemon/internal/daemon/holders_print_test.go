package daemon

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// The box print test of this package's holders (oauth2calltest.PrintTest): githubText, and the
// types the daemon decodes GitHub's answers into that hold it - the base rules read's, the pull
// request's, and the merge-state query's (graphQLPull, the query's answer, and the chunk of
// answers a base's read keeps) - each holding the probe in every box it reaches and in githubText's
// member as it came, and each twin the same shape with the probe as a string in each of those
// places.
func TestEachHolderPrintsNoSecretByAnyPrinterAtAnyPlacement(t *testing.T) {
	type textTwin struct {
		Name string
		Said string
	}
	type checkTwin struct {
		Context textTwin `json:"context"`
	}
	type checksTwin struct {
		RequiredStatusChecks []checkTwin `json:"required_status_checks"`
		Strict               bool        `json:"strict_required_status_checks_policy"`
	}
	type ruleTwin struct {
		Type       string     `json:"type"`
		Parameters checksTwin `json:"parameters"`
	}
	type protectionChecksTwin struct {
		EnforcementLevel string      `json:"enforcement_level"`
		Contexts         []textTwin  `json:"contexts"`
		Checks           []checkTwin `json:"checks"`
	}
	type protectionTwin struct {
		Enabled              bool                 `json:"enabled"`
		RequiredStatusChecks protectionChecksTwin `json:"required_status_checks"`
	}
	type branchTwin struct {
		Protection protectionTwin `json:"protection"`
	}
	type baseTwin struct {
		Ref textTwin `json:"ref"`
	}
	type pullTwin struct {
		Number  int      `json:"number"`
		State   string   `json:"state"`
		HTMLURL string   `json:"html_url"`
		Body    string   `json:"body"`
		Base    baseTwin `json:"base"`
		Head    struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		UpdatedAt      time.Time `json:"updated_at"`
		Mergeable      *bool     `json:"mergeable"`
		MergeableState string    `json:"mergeable_state"`
		Merged         bool      `json:"merged"`
		MergeCommitSHA string    `json:"merge_commit_sha"`
	}
	type graphQLPullTwin struct {
		Number           int      `json:"number"`
		State            string   `json:"state"`
		HeadRefOid       string   `json:"headRefOid"`
		HeadRefName      string   `json:"headRefName"`
		BaseRefName      textTwin `json:"baseRefName"`
		Mergeable        string   `json:"mergeable"`
		MergeStateStatus string   `json:"mergeStateStatus"`
	}
	type queryAnswerTwin struct {
		Repository map[string]*graphQLPullTwin `json:"repository"`
	}
	type chunkTwin struct {
		github     githubrest.Client
		answers    map[int]graphQLPullTwin
		unanswered map[int]string
		readAt     time.Time
		unminted   bool
		timedOut   bool
	}
	type chunkExported struct {
		Github     githubrest.Client
		Answers    map[int]graphQLPullTwin
		Unanswered map[int]string
		ReadAt     time.Time
		Unminted   bool
		TimedOut   bool
	}
	// githubText holds the probe as it came and as GitHub's words, as its decode of an answer that
	// names the probe fills it.
	var text githubText
	if err := json.Unmarshal([]byte(strconv.Quote(oauth2calltest.Words)), &text); err != nil {
		t.Fatal(err)
	}
	textT := textTwin{Name: oauth2calltest.Words, Said: oauth2calltest.Words}
	check, checkT := requiredCheck{Context: text}, checkTwin{Context: textT}
	checks := requiredStatusChecks{RequiredStatusChecks: []requiredCheck{check}, Strict: true}
	checksT := checksTwin{RequiredStatusChecks: []checkTwin{checkT}, Strict: true}
	protectionChecksValue := protectionChecks{EnforcementLevel: "non_admins", Contexts: []githubText{text}, Checks: []requiredCheck{check}}
	protectionChecksT := protectionChecksTwin{EnforcementLevel: "non_admins", Contexts: []textTwin{textT}, Checks: []checkTwin{checkT}}
	protection := branchProtection{Enabled: true, RequiredStatusChecks: protectionChecksValue}
	protectionT := protectionTwin{Enabled: true, RequiredStatusChecks: protectionChecksT}
	base, baseT := pullRequestBase{Ref: text}, baseTwin{Ref: textT}
	pull := githubPullRequest{Number: 86, State: "open", Base: base}
	pullT := pullTwin{Number: 86, State: "open", Base: baseT}
	node := graphQLPull{Number: 86, State: "OPEN", HeadRefOid: "code", BaseRefName: text, Mergeable: "UNKNOWN", MergeStateStatus: "UNKNOWN"}
	nodeT := graphQLPullTwin{Number: 86, State: "OPEN", HeadRefOid: "code", BaseRefName: textT, Mergeable: "UNKNOWN", MergeStateStatus: "UNKNOWN"}
	queryAnswer := struct {
		Repository map[string]*graphQLPull `json:"repository"`
	}{Repository: map[string]*graphQLPull{"p86": &node}}
	queryAnswerT := queryAnswerTwin{Repository: map[string]*graphQLPullTwin{"p86": &nodeT}}
	chunk := chunkAnswer{answers: map[int]graphQLPull{86: node}, unanswered: map[int]string{}}
	chunkT := chunkTwin{answers: map[int]graphQLPullTwin{86: nodeT}, unanswered: map[int]string{}}
	chunkE := chunkExported{Answers: map[int]graphQLPullTwin{86: nodeT}, Unanswered: map[int]string{}}
	for _, h := range []oauth2calltest.Holder{
		{Name: "daemon.githubText", Kind: "fields", Boxed: text, Same: textT, Exported: textT},
		{Name: "daemon.requiredCheck", Kind: "fields", Boxed: check, Same: checkT, Exported: checkT},
		{Name: "daemon.requiredStatusChecks", Kind: "fields", Boxed: checks, Same: checksT, Exported: checksT},
		{Name: "daemon.githubBranchRule", Kind: "fields",
			Boxed: githubBranchRule{Type: "required_status_checks", Parameters: checks},
			Same:  ruleTwin{Type: "required_status_checks", Parameters: checksT}, Exported: ruleTwin{Type: "required_status_checks", Parameters: checksT}},
		{Name: "daemon.protectionChecks", Kind: "fields", Boxed: protectionChecksValue, Same: protectionChecksT, Exported: protectionChecksT},
		{Name: "daemon.branchProtection", Kind: "fields", Boxed: protection, Same: protectionT, Exported: protectionT},
		{Name: "daemon.githubBranch", Kind: "fields", Boxed: githubBranch{Protection: protection},
			Same: branchTwin{Protection: protectionT}, Exported: branchTwin{Protection: protectionT}},
		{Name: "daemon.pullRequestBase", Kind: "fields", Boxed: base, Same: baseT, Exported: baseT},
		{Name: "daemon.githubPullRequest", Kind: "fields", Boxed: pull, Same: pullT, Exported: pullT},
		{Name: "daemon.graphQLPull", Kind: "fields", Boxed: node, Same: nodeT, Exported: nodeT},
		{Name: "daemon.queryChunk.answer", Kind: "fields", Boxed: queryAnswer, Same: queryAnswerT, Exported: queryAnswerT},
		{Name: "daemon.chunkAnswer", Kind: "struct", Boxed: chunk, Same: chunkT, Exported: chunkE},
	} {
		t.Run(h.Name, func(t *testing.T) { oauth2calltest.PrintTest(t, h) })
	}
}
