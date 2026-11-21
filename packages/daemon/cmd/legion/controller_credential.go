package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// controllerGitHubRefreshInterval is how often startControllerGitHubRefresh re-fetches the
// controller's GitHub credential. The daemon's token source holds the review App's leases and
// re-mints each as it nears its expiry, so most ticks render the hosts.yml files the controller's
// directories already hold and write nothing; a minute keeps the time its gh runs on a token the
// daemon has already replaced short against an installation token's hour, as tmux's and sandbox's
// gitHubRefreshInterval do for the roles the daemon launches itself. A var, not a const, so a test
// can shorten it.
var controllerGitHubRefreshInterval = time.Minute

// controllerCredentialFailure is a non-2xx answer to POST
// /legion/v1/controller/github-credential whose body parsed as an api.Failure: its code lets the
// caller tell a daemon with no GitHub App to act as (GITHUB_TOKEN_SOURCE_UNAVAILABLE,
// GITHUB_OWNER_UNCONFIGURED) apart from a capability a later start replaced
// (INVALID_CONTROLLER_CAPABILITY) or a daemon that launches its own controller
// (CONTROLLER_LAUNCHED), without parsing the sentence itself. The code is one of those four
// (controllerCredentialCodes), matched with the daemon's code as it came, and "" for any other.
type controllerCredentialFailure struct {
	code    string
	message string
}

func (f *controllerCredentialFailure) Error() string { return f.message }

// controllerCredentialCodes are the codes of the daemon's credential refusals this command
// decides on (controllerStartGitHubCredential, controllerCredentialSuperseded).
var controllerCredentialCodes = []string{"GITHUB_TOKEN_SOURCE_UNAVAILABLE", "GITHUB_OWNER_UNCONFIGURED", "INVALID_CONTROLLER_CAPABILITY", "CONTROLLER_LAUNCHED"}

// controllerCredentialSuperseded says whether err is a controllerCredentialFailure that ends the
// refresh loop rather than retries it: the capability a later `legion controller start` replaced
// (INVALID_CONTROLLER_CAPABILITY), or a daemon that now launches its own controller
// (CONTROLLER_LAUNCHED). Either means this process is no longer the daemon's controller, so its gh
// files are left to last until their token's expiry — last start wins.
func controllerCredentialSuperseded(err error) bool {
	var failure *controllerCredentialFailure
	return errors.As(err, &failure) && (failure.code == "INVALID_CONTROLLER_CAPABILITY" || failure.code == "CONTROLLER_LAUNCHED")
}

// fetchControllerCredential is `legion controller start`'s refresh call, POST
// /legion/v1/controller/github-credential: the controller capability secret proves which
// controller is asking, defaultOwner names the owner whose directory its GH_CONFIG_DIR names (empty
// on the first fetch, which the daemon answers with the first owner), and the daemon answers the
// review App's read-only token for every owner that App is installed on, and for an owner a
// configured rename moved a repository away from, each rendered as the two files gh
// reads, and the configured renames its gh routes a moved repository by, as a gh bundle
// (ghconfig.Bundle) whose every owner carries the answer's App and its
// expiry, parsed from the API's RFC 3339 nanosecond timestamp (time.RFC3339Nano, the format
// api.ControllerCredentialResponse's ExpiresAt is written in). Beside the bundle it answers
// installed, the owner accounts the daemon read the review App installed on as it built that
// bundle, lowercased and sorted (api.ControllerCredentialResponse's Installed), which the
// first fetch reads for the controller's addressing (controllerReach) and a refresh does not
// read. An answer that is no valid bundle,
// a rename of it naming a repository GitHub could hold no name for included, or whose App is
// none of the App roles (appauth.Roles), is an error, which names the member of the answer it
// read and never its value: a parser's error quotes the value it could not read, and every value
// of the answer is the daemon's word. A failed request names the daemon URL, as
// fetchControllerSecret's does, and never tries another address. A refusal is a
// *controllerCredentialFailure carrying the daemon's code when the body parses as one
// (api.Failure), quoting its sentence through refusal otherwise.
func fetchControllerCredential(ctx context.Context, daemonURL, secret, defaultOwner string) (bundle ghconfig.Bundle, installed []string, err error) {
	const route = "/legion/v1/controller/github-credential"
	answered, err := operator{base: daemonURL}.do(ctx, http.MethodPost, route, api.ControllerCredentialRequest{Secret: secret, Default: defaultOwner})
	if err != nil {
		return ghconfig.Bundle{}, nil, fmt.Errorf("could not reach the Legion daemon at %s: %v; is the port-forward running? (never falls back to another address)", daemonURL, err)
	}
	if answered.Status()/100 != 2 {
		// Which refusal it is, is decided on the daemon's code as it came (RemoteText.Equal), and
		// never on the rule's reading of it (SendBack), which is "" where a value the request
		// carried shares a run of characters with the code; the sentence prints as the rule reads
		// it (oauth2call.CodedRefusal, the shape of api.Failure).
		var failure oauth2call.CodedRefusal
		if answered.Decode(&failure) == nil && !failure.Code.IsEmpty() {
			code := ""
			if i := slices.IndexFunc(controllerCredentialCodes, failure.Code.Equal); i >= 0 {
				code = controllerCredentialCodes[i]
			}
			return ghconfig.Bundle{}, nil, &controllerCredentialFailure{
				code:    code,
				message: fmt.Sprintf("%s%s: %s", daemonURL, route, failure.Error),
			}
		}
		return ghconfig.Bundle{}, nil, fmt.Errorf("%s%s: %s", daemonURL, route, refusal(answered))
	}
	var answer api.ControllerCredentialResponse
	if err := answered.Decode(&answer); err != nil {
		return ghconfig.Bundle{}, nil, fmt.Errorf("%s%s answered with a body that is not JSON", daemonURL, route)
	}
	if !slices.Contains(appauth.Roles, appauth.AppRole(answer.App)) {
		return ghconfig.Bundle{}, nil, fmt.Errorf("%s%s answered an App (app) that is none of the App roles this legion knows, %s and %s", daemonURL, route, appauth.Implement, appauth.Review)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, answer.ExpiresAt)
	if err != nil {
		return ghconfig.Bundle{}, nil, fmt.Errorf("%s%s answered an expiry (expiresAt) that is not RFC 3339", daemonURL, route)
	}
	bundle = ghconfig.Bundle{Default: answer.Default, Dirs: make(map[string]ghconfig.Rendered, len(answer.Owners))}
	for owner, files := range answer.Owners {
		bundle.Dirs[owner] = ghconfig.Rendered{Hosts: files.Hosts, Config: files.Config, App: answer.App, ExpiresAt: expiresAt}
	}
	for i, pair := range answer.Renames {
		from, err := ghrepo.Parse("the repository a rename moved", pair.From)
		if err != nil {
			return ghconfig.Bundle{}, nil, fmt.Errorf("%s%s answered a rename whose from (renames[%d].from) names no repository GitHub could hold", daemonURL, route, i)
		}
		to, err := ghrepo.Parse("the repository a rename moved it to", pair.To)
		if err != nil {
			return ghconfig.Bundle{}, nil, fmt.Errorf("%s%s answered a rename whose to (renames[%d].to) names no repository GitHub could hold", daemonURL, route, i)
		}
		bundle.Pairs = append(bundle.Pairs, ghrepo.Rename{From: from, To: to})
	}
	if bundle.Validate() != nil {
		return ghconfig.Bundle{}, nil, fmt.Errorf("%s%s answered a credential that is no gh bundle: its default and each of its owners (default, owners) must be a lowercased GitHub login, and its default one of its owners", daemonURL, route)
	}
	return bundle, answer.Installed, nil
}

// controllerGHDir is the root of the controller's gh bundle within stateDir, its
// LEGION_GH_OWNERS_DIR: one directory per owner (ghconfig.Dir), the default owner's its
// GH_CONFIG_DIR.
func controllerGHDir(stateDir string) string { return filepath.Join(stateDir, "gh") }

// controllerReach is the owners the controller's addressing names (daemon.RepositoriesSentence,
// which says of each that the role's GitHub App is installed on it and that its token reaches
// every repository of it): the owners of installed, the accounts the daemon read the review App
// installed on, that bundle also holds a directory for, in installed's order, which is sorted.
//
// Both are consulted because each proves one half of that sentence and neither proves both.
// The bundle's directories prove the token and not the installation: a bundle also holds a
// directory for an owner a configured rename moved a repository away from, an alias, whose
// token is the lease of the owner the repository moved to, on an account the App may not be
// installed on at all. An alias is never named, because it is not in installed. The
// installations prove the installation and not the token: an owner can be in installed while
// GitHub refused its reading lease, so the bundle holds no directory for it (appauth's
// mintBundle), and "its token reaches every repository of those owners" would be false of that
// owner. The installed owners the bundle also holds a directory for are the only set the whole
// sentence is true of.
func controllerReach(installed []string, bundle ghconfig.Bundle) []string {
	reach := make([]string, 0, len(installed))
	for _, owner := range installed {
		if _, held := bundle.Dirs[owner]; held {
			reach = append(reach, owner)
		}
	}
	return reach
}

// controllerStartGitHubCredential is the first fetch controllerStart makes after it writes the
// secret file: success writes the controller's gh bundle under controllerGHDir(stateDir)
// (ghconfig.WriteBundle), names its App and directories on stderr, starts the refresh loop,
// logging to stateDir/github-credential.log, and answers the bundle's default owner, whose
// directory the controller's GH_CONFIG_DIR names, and reach, the owners the controller's
// addressing names (controllerReach); a daemon with no GitHub App for this project
// (GITHUB_TOKEN_SOURCE_UNAVAILABLE or GITHUB_OWNER_UNCONFIGURED) is not a refusal, since nothing
// is wrong — it just has nothing to act as — so this removes everything an earlier run left in the
// gh directory, leaving it empty 0700, no loop running, no default and no owner, so a token an
// earlier run minted against a daemon that had an App never outlives a start that says gh acts as
// nobody; any other failure is this function's error, which controllerStart treats as every other
// post-mint failure (exit 1, Oh My Pi never started, the secret file left as it is). It answers a
// function that stops the loop — a no-op when none started — for controllerStart to defer.
func controllerStartGitHubCredential(ctx context.Context, daemonURL, secret, stateDir string, stderr io.Writer) (stop func(), defaultOwner string, reach []string, err error) {
	ghDir := controllerGHDir(stateDir)
	noop := func() {}
	bundle, installed, err := fetchControllerCredential(ctx, daemonURL, secret, "")
	if err != nil {
		var failure *controllerCredentialFailure
		if !errors.As(err, &failure) || (failure.code != "GITHUB_TOKEN_SOURCE_UNAVAILABLE" && failure.code != "GITHUB_OWNER_UNCONFIGURED") {
			return noop, "", nil, err
		}
		if err := ghconfig.EmptyBundle(ghDir); err != nil {
			return noop, "", nil, fmt.Errorf("empty %s: %w", ghDir, err)
		}
		fmt.Fprintln(stderr, "[legion] the daemon has no GitHub App to act as; the controller's gh acts as nobody")
		return noop, "", nil, nil
	}
	if _, err := ghconfig.WriteBundle(ghDir, bundle); err != nil {
		return noop, "", nil, fmt.Errorf("write the controller's gh files: %w", err)
	}
	written := bundle.Dirs[bundle.Default]
	fmt.Fprintf(stderr, "[legion] the controller's gh acts as the %s App for owners %s (read-only) from %s; this process refreshes the tokens before they expire (%s)\n",
		written.App, strings.Join(bundle.DirNames(), ", "), ghDir, written.ExpiresAt.Format(time.RFC3339))
	stop, err = startControllerGitHubRefresh(daemonURL, secret, ghDir, bundle.Default, filepath.Join(stateDir, "github-credential.log"))
	if err != nil {
		return noop, "", nil, err
	}
	return stop, bundle.Default, controllerReach(installed, bundle), nil
}

// startControllerGitHubRefresh starts the loop that keeps the controller's gh bundle fresh while
// Oh My Pi runs: a goroutine that re-fetches the controller's GitHub credential for defaultOwner,
// the owner its GH_CONFIG_DIR names, every controllerGitHubRefreshInterval and rewrites dir
// (ghconfig.WriteBundle), which rewrites only the owners whose hosts.yml differs. The loop writes
// nothing to stderr or stdout once Oh My Pi owns the terminal: it appends one slog text line per
// change, per failed fetch or write, and per stop the daemon's refusal causes to logPath, opened
// append-only 0600 before Oh My Pi starts, and never logs a token or the hosts text; Oh My Pi's
// exit ends the loop silently, with no line for it. A failed fetch or write, and an answer whose
// default is not defaultOwner, keep the last files and try again at the next tick; a superseded
// capability (controllerCredentialSuperseded) ends the loop, since the files then last until their
// tokens' expiry. Its lifetime is Oh My Pi's, not the command's signal context: a Ctrl-C in the
// terminal reaches both processes, and Oh My Pi, which handles it as its own, keeps running, so a
// loop bound to the signal would leave the controller on tokens no one refreshes. It answers a
// function that cancels the loop and waits for it to stop, which controllerStart runs once
// Oh My Pi has exited, so the process never exits while a write is in flight.
func startControllerGitHubRefresh(daemonURL, secret, dir, defaultOwner, logPath string) (stop func(), err error) {
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", logPath, err)
	}
	log := slog.New(slog.NewTextHandler(logFile, redact.HandlerOptions(nil)))
	loopCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer logFile.Close()
		tick := time.NewTicker(controllerGitHubRefreshInterval)
		defer tick.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-tick.C:
				if !refreshControllerGitHubCredential(loopCtx, daemonURL, secret, dir, defaultOwner, log) {
					return
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}, nil
}

// refreshControllerGitHubCredential is one tick of startControllerGitHubRefresh's loop. It
// answers false when the loop must stop (a superseded capability), true otherwise.
func refreshControllerGitHubCredential(ctx context.Context, daemonURL, secret, dir, defaultOwner string, log *slog.Logger) bool {
	bundle, _, err := fetchControllerCredential(ctx, daemonURL, secret, defaultOwner)
	if err != nil {
		if controllerCredentialSuperseded(err) {
			log.Info("controller: github credential refresh stopped", "reason", err.Error())
			return false
		}
		log.Warn("controller: github credential refresh failed", "error", err.Error())
		return true
	}
	if bundle.Default != defaultOwner {
		log.Warn("controller: github credential refresh failed", "error",
			fmt.Sprintf("the credential's default owner %s is not the controller's %s", bundle.Default, defaultOwner))
		return true
	}
	changed, err := ghconfig.WriteBundle(dir, bundle)
	if err != nil {
		log.Warn("controller: github credential refresh failed", "error", err.Error())
		return true
	}
	if len(changed) > 0 {
		written := bundle.Dirs[defaultOwner]
		log.Info("controller: github credential refreshed", "app", written.App, "expiresAt", written.ExpiresAt, "owners", changed)
	}
	return true
}
