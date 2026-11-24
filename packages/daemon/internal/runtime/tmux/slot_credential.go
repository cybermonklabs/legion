package tmux

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var _ runtime.SlotCredentials = (*Runtime)(nil)

// WriteSlotCredential satisfies runtime.SlotCredentials: the pane of token's claim, while it is
// watched, has its gh files written for the App of the role it works as (writePaneCredential, the
// refresh's write), judged under the launch lock with the write, built by appauth.Bundles on the
// pane's claim's issue, so a role a plan defines holds the App of its type. Only the directories
// whose hosts.yml differs are rewritten. A runtime with no credential function wrote no pane any
// gh files, and a claim with no watched pane has none to write: its next spawn writes the App
// runtime.SlotRole answers then, judged under the same lock.
func (r *Runtime) WriteSlotCredential(ctx context.Context, token claim.Token) error {
	if r.gitHubCredential == nil {
		return nil
	}
	bundles := appauth.NewBundles(r.gitHubCredential, r.controllerCredential, r.roleApps)
	for _, entry := range r.trackedProcesses() {
		if entry.locator.Claim != token {
			continue
		}
		written, err := r.writePaneCredential(ctx, bundles, entry)
		if err != nil {
			return fmt.Errorf("write the pane of %s for %s: %w", token, written.role, err)
		}
		if !written.watched {
			continue
		}
		if len(written.changed) > 0 {
			held := written.bundle.Dirs[written.bundle.Default]
			r.log.Info("tmux runtime: github credential written for a step", "claim", token, "role", written.role, "app", held.App,
				"expiresAt", held.ExpiresAt, "dirs", written.changed)
		}
		return nil
	}
	return nil
}

// SlotHoldsCredential satisfies runtime.SlotCredentials: it renders role's App for the watched pane
// of token's claim (renderPane, which writes nothing) and reads, under the launch lock every write
// of the claim's gh directory is made under, the token the hosts.yml of the render's default owner
// holds in that directory. It answers nil where that is the render's token, so once every write
// begun before it has landed; runtime.ErrSlotCredentialPending, a wait, while the files hold
// another token or none, or no pane of the claim is watched to read them; and nil for a runtime
// with no credential function, or a render whose gh acts as nobody, which hold nothing to wait for.
func (r *Runtime) SlotHoldsCredential(ctx context.Context, token claim.Token, role claim.Role) error {
	if r.gitHubCredential == nil {
		return nil
	}
	bundles := appauth.NewBundles(r.gitHubCredential, r.controllerCredential, r.roleApps)
	for _, entry := range r.trackedProcesses() {
		if entry.locator.Claim != token {
			continue
		}
		bundle, err := r.renderPane(ctx, bundles, entry, role)
		if err != nil {
			return fmt.Errorf("render the pane of %s for %s: %w", token, role, err)
		}
		if bundle.Default == "" {
			return nil
		}
		want, err := ghconfig.TokenFromHosts([]byte(bundle.Dirs[bundle.Default].Hosts))
		if err != nil {
			return fmt.Errorf("the credential of %s for %s: %w", token, role, err)
		}
		held, watched, err := r.watchedHostsToken(entry, bundle.Default)
		if err != nil {
			return fmt.Errorf("read the gh files of the pane of %s: %w", token, err)
		}
		if !watched {
			continue
		}
		if held != want {
			return fmt.Errorf("the gh files of the pane of %s do not hold %s's credential yet: %w", token, role, runtime.ErrSlotCredentialPending)
		}
		return nil
	}
	return fmt.Errorf("no pane of %s is watched whose gh files can be read: %w", token, runtime.ErrSlotCredentialPending)
}

// watchedHostsToken is the token the hosts.yml of owner's directory in the gh directory of entry's
// claim holds, read under the launch lock while entry's pane is still watched, and false where it
// is not. A hosts.yml that is not there, or names no token, holds "".
func (r *Runtime) watchedHostsToken(entry trackedProcess, owner string) (string, bool, error) {
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	if !r.watching(entry) {
		return "", false, nil
	}
	data, err := os.ReadFile(filepath.Join(ghconfig.Dir(runtime.GHConfigDir(r.stateDir, entry.locator.Claim), owner), ghconfig.HostsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", true, nil
	}
	if err != nil {
		return "", true, err
	}
	held, _ := ghconfig.TokenFromHosts(data)
	return held, true, nil
}
