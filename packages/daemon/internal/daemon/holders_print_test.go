package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
)

// The box print test of this package's holders (oauth2calltest.PrintTest): githubText, and the
// types the daemon decodes GitHub's answers into that hold it - the base rules read's and the pull
// request's - each holding the probe in every box it reaches, and each twin the same shape with
// the probe as a string there.
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
	text := githubText{Name: "lint", Said: oauth2calltest.Said(oauth2calltest.Words)}
	textT := textTwin{Name: "lint", Said: oauth2calltest.Words}
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
	} {
		t.Run(h.Name, func(t *testing.T) { oauth2calltest.PrintTest(t, h) })
	}
}
