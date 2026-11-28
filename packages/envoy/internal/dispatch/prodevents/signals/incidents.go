package signals

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The incident_issue signal (dispatch://INFRA-978, R7 and D5): an issue labelled incident whose
// own links (issue_external_links) name a pull request or a workflow run is a failure of that pull
// request's deploy or that run's deploy row: the cause the issue names, not the pull requests that
// cite the issue, which are its fix. It is written under the issue's project, keyed by the issue
// (IncidentSourceRef), its actor and detected time those of the event that made the issue an
// incident or gave it its link, and only where the deploy it blames counts for the issue's project
// (deployCountsForRow). An issue whose blamed deploy counts for other projects alone writes no row
// and stays refused on that deploy: its refusal commits in the transaction that asked, with a log
// line (incident_refusals, migration 20261128030142), so no later event of the issue writes a row
// blaming that deploy, not even once a source move, a rule change or a default-repository move has
// made it count for the issue's project
// (dispatch://INFRA-978/comment/d57e8d06-afb3-4456-9e3d-516923e2b706). A bug label makes none.

// incidentLabel is the label that makes an issue an incident, in any case.
const incidentLabel = "incident"

// incidentEvents is the condition on the alias event of the issue events the incidents feed reads:
// an issue.created or issue.updated whose issue holds the incident label after it, and which
// created the issue, moved its labels (the label gained) or moved its links (a link added to an
// incident). jsonb_path_exists reads a payload whose labels are no array as holding none.
const incidentEvents = `jsonb_path_exists(event.payload, '$.labels[*] ? (@ like_regex "^` + incidentLabel + `$" flag "i")')
	and (event.type = 'issue.created' or coalesce((event.payload -> 'changed') ?| array['labels', 'external_links'], false))`

// IncidentSourceRef is the source_ref of the incident_issue row of issue: incident:<issue key>, one
// row per incident whatever links it gains later.
func IncidentSourceRef(issue string) string {
	return prodevents.IncidentSourceRefPrefix + issue
}

// passIncidents is one pass of the incidents feed (passEventFeed): it reads the issue events that
// make or move an incident (incidentEvents) and writes each one's incident_issue (recordIncident).
func passIncidents(ctx context.Context, pool *store.Pool, safe int64) error {
	return passEventFeed(ctx, pool, incidentsCursor, safe, []string{"issue.created", "issue.updated"}, incidentEvents,
		func(event feedEvent) error {
			_, err := recordIncident(ctx, pool, event.issueKey, event.createdAt, event.actor)
			return err
		})
}

// recordIncident writes the incident_issue of issue, an incident, from the links it holds now, and
// reports whether it wrote a row. The failure blames the earliest deploy among those its links
// name, of any project but a deploy another project posted: a pull request's (the deploy row that
// names it among the pull requests it shipped, else the one whose commits hold its stored merge
// commit) and a run's (the run's deploy row, the GitHub adapter's). Its pull requests are the pull
// requests its links name, or the blamed deploy's where they name only runs. An issue whose links
// name no deployed pull request and no deploy run writes nothing until a link that does is added.
// One whose blamed deploy counts for other projects than the issue's alone writes nothing either,
// and its refusal of that deploy commits in the transaction that asked (writeSignal,
// incidentRefusal): a later event that blames the same deploy writes nothing, and a link that
// makes another deploy the blamed one, which counts for the issue's project, writes its row. One
// whose blamed deploy counts for no project writes nothing and is not refused, so a later event
// asks again. A repeat writes nothing, and an incident whose row is made already reads no link.
func recordIncident(ctx context.Context, pool *store.Pool, issue string, detected time.Time, actor model.Actor) (bool, error) {
	var project string
	if err := pool.QueryRow(ctx, `select project_key from issues where key = $1`, issue).Scan(&project); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read issue %s: %w", issue, err)
	}
	ref := IncidentSourceRef(issue)
	if _, made, err := prodevents.BySourceRef(ctx, pool, project, ref); err != nil || made {
		return false, err
	}
	links, err := issueLinks(ctx, pool, issue)
	if err != nil {
		return false, err
	}
	var blamed *prodevents.Event
	pulls := []string{}
	for _, link := range links {
		target, ok := parseLink(link)
		if !ok {
			continue
		}
		var deploy prodevents.Event
		var held bool
		if target.pull > 0 {
			name := delivery.PullRequestName(target.repo, target.pull)
			pulls = append(pulls, name)
			if deploy, held, err = pullRequestDeploy(ctx, pool, project, target.repo, target.pull); err != nil {
				return false, err
			}
		} else if deploy, held, err = prodevents.DeployForRun(ctx, pool, target.repo, target.run); err != nil {
			return false, err
		}
		if held && (blamed == nil || deploy.FinishedAt.Before(*blamed.FinishedAt)) {
			blamed = &deploy
		}
	}
	if blamed == nil {
		return false, nil
	}
	var refused bool
	if err := pool.QueryRow(ctx, `select exists (select from incident_refusals where issue_key = $1 and deploy_id = $2::uuid)`,
		issue, blamed.ID).Scan(&refused); err != nil {
		return false, fmt.Errorf("read the refusals of %s's incident_issue: %w", issue, err)
	}
	if refused {
		return false, nil
	}
	if len(pulls) == 0 {
		pulls = blamed.PullRequests
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin the incident_issue of %s: %w", issue, err)
	}
	defer tx.Rollback(ctx)
	created, err := writeSignal(ctx, pool, tx, prodevents.Event{
		ProjectKey: project, Kind: prodevents.KindFailure, Source: prodevents.SourceIncidentLabel,
		SourceRef: ref, PullRequests: pulls, Actor: actor,
		DeployID: new(blamed.ID), Signal: new(prodevents.SignalIncidentIssue), DetectedAt: new(detected.UTC()),
	}, incidentRefusal(issue), nil)
	if err != nil {
		return false, fmt.Errorf("write the incident_issue of %s: %w", issue, err)
	}
	return created, nil
}

// incidentRefusal is issue's refusal of a deploy its links blame (incident_refusals): a later event
// of the issue whose links blame the same deploy writes no row, and one whose links blame another
// deploy asks again.
func incidentRefusal(issue string) refusal {
	return refusal{signal: prodevents.SignalIncidentIssue, key: issue, record: func(ctx context.Context, tx pgx.Tx, deploy string) error {
		if _, err := tx.Exec(ctx, `insert into incident_refusals (issue_key, deploy_id) values ($1, $2::uuid)
			on conflict do nothing`, issue, deploy); err != nil {
			return fmt.Errorf("refuse %s the incident_issue on deploy %s: %w", issue, deploy, err)
		}
		return nil
	}}
}

// issueLinks is the urls of issue's own links, in order.
func issueLinks(ctx context.Context, q delivery.Querier, issue string) ([]string, error) {
	rows, err := q.Query(ctx, `select url from issue_external_links where issue_key = $1 order by url`, issue)
	if err != nil {
		return nil, fmt.Errorf("read the links of %s: %w", issue, err)
	}
	defer rows.Close()
	var links []string
	for rows.Next() {
		var link string
		if err := rows.Scan(&link); err != nil {
			return nil, fmt.Errorf("read the links of %s: %w", issue, err)
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// pullRequestDeploy is the deploy of repo#number that a signal of project may blame: the deploy row
// that names it among the pull requests it shipped, else the one whose commits hold its merge
// commit as delivery_pull_requests stores it, and false where neither holds it
// (prodevents.DeployForPullRequest, prodevents.DeployForCommit).
func pullRequestDeploy(ctx context.Context, q delivery.Querier, project, repo string, number int) (prodevents.Event, bool, error) {
	deploy, held, err := prodevents.DeployForPullRequest(ctx, q, project, delivery.PullRequestName(repo, number))
	if err != nil || held {
		return deploy, held, err
	}
	var sha *string
	err = q.QueryRow(ctx, `select merge_commit_sha from delivery_pull_requests where lower(repo) = lower($1) and number = $2`,
		repo, number).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (sha == nil || *sha == "") {
		return prodevents.Event{}, false, nil
	}
	if err != nil {
		return prodevents.Event{}, false, fmt.Errorf("read the merge commit of %s#%d: %w", repo, number, err)
	}
	return prodevents.DeployForCommit(ctx, q, project, *sha)
}

// linkTarget is what a link names: a pull request (pull above zero) or a workflow run (run above
// zero) of repo, which is in lower case.
type linkTarget struct {
	repo string
	pull int
	run  int64
}

// pullNumber is a pull request's number as a link names it, bounded to nine digits as a linked
// pull request's is (delivery's linkedPullRequestTerms): delivery_pull_requests keeps it in an
// integer column, so a read of a larger one would fail every pass and hold the cursor where it
// stands.
var pullNumber = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

// parseLink reads the pull request or the workflow run a GitHub link names: a pull request's page,
// https://github.com/<owner>/<repo>/pull/<n> (n of at most nine digits, pullNumber) and any page
// under it, and a run's, https://github.com/<owner>/<repo>/actions/runs/<id> and any page under it
// (a job's, an attempt's). ok is false for any other link.
func parseLink(link string) (linkTarget, bool) {
	parsed, err := url.Parse(strings.TrimSpace(link))
	if err != nil || !strings.EqualFold(parsed.Hostname(), "github.com") && !strings.EqualFold(parsed.Hostname(), "www.github.com") {
		return linkTarget{}, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 4 {
		return linkTarget{}, false
	}
	repo := parts[0] + "/" + parts[1]
	if _, _, err := refs.SplitGitHubRepo(repo); err != nil {
		return linkTarget{}, false
	}
	target := linkTarget{repo: strings.ToLower(repo)}
	switch {
	case parts[2] == "pull":
		if !pullNumber.MatchString(parts[3]) {
			return linkTarget{}, false
		}
		target.pull, _ = strconv.Atoi(parts[3])
	case parts[2] == "actions" && parts[3] == "runs" && len(parts) >= 5:
		run, err := strconv.ParseInt(parts[4], 10, 64)
		if err != nil || run <= 0 {
			return linkTarget{}, false
		}
		target.run = run
	default:
		return linkTarget{}, false
	}
	return target, true
}
