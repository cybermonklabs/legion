package repocache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/rolefile"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// removeCloneAfterNarrowingOutOfSpace removes a clone whose narrowing repack could not fit, so the
// next fetch replaces its old packs with a fresh clone of the default branch. Other narrowing
// failures keep the clone unavailable for diagnosis and retry.
func (c *cache) removeCloneAfterNarrowingOutOfSpace(err error) error {
	if !errors.Is(err, errNarrowingOutOfSpace) {
		return err
	}
	if removeErr := rolefile.RemoveTree(c.dir); removeErr != nil {
		return fmt.Errorf("%w; remove the cache clone after its narrowing repack ran out of space: %w", err, removeErr)
	}
	c.opts.Log("removed the cache clone after its narrowing repack ran out of space; the next fetch clones it afresh")
	return err
}

const (
	defaultBranchOnlyFile  = ".legion-default-branch-only"
	defaultBranchFetchFile = ".legion-default-branch-fetching"
)

var (
	errDefaultBranchOnly   = errors.New("the repository cache clone is not narrowed to its default branch")
	errCacheHeadMissing    = errors.New("the repository cache's HEAD branch is missing")
	errNarrowingOutOfSpace = errors.New("the narrowing repack ran out of space")
)

func (c *cache) narrow(ctx context.Context, freshClone, rewritten bool) (err error) {
	c.narrowed = false
	defer func() { c.narrowed = err == nil }()
	head, err := workspace.CacheGit(ctx, c.opts.Run, c.dir, "symbolic-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("read the cache's HEAD branch: %w", err)
	}
	branch := strings.TrimSuffix(head.Stdout, "\n")
	if !strings.HasPrefix(branch, "refs/heads/") {
		return fmt.Errorf("the cache's HEAD names %q, not a branch", branch)
	}
	refs, err := workspace.CacheGit(ctx, c.opts.Run, c.dir, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return fmt.Errorf("list the cache's refs: %w", err)
	}
	var (
		hasHead bool
		remove  []string
	)
	for _, ref := range strings.Split(strings.TrimSuffix(refs.Stdout, "\n"), "\n") {
		if ref == "" {
			continue
		}
		if ref == branch {
			hasHead = true
			continue
		}
		remove = append(remove, ref)
	}
	if !hasHead {
		return fmt.Errorf("%w: %s", errCacheHeadMissing, branch)
	}
	marked, err := c.defaultBranchOnlyMarked()
	if err != nil {
		return err
	}
	if len(remove) == 0 && marked && !rewritten {
		return nil
	}
	if len(remove) == 0 && freshClone && !rewritten {
		return c.markDefaultBranchOnly()
	}
	// The repack holds the write lock /tar waits on for up to repackTimeout. A cache that answered
	// ready would pass every pod's wait and stall each one on /tar until its stream bound ends it, so
	// the cache answers unready now; a pod waits for it as it waits for a cache not yet narrowed. The
	// set /packs lists was made before this narrowing, so it goes too: /packs answers none until the
	// sync after the narrowing lists the narrowed packs (syncPacks).
	c.health.Store(&Health{Repo: c.opts.Repo.String()})
	c.listed.Store(nil)
	before, err := c.objectBytes()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(c.opts.Root, defaultBranchOnlyFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the old default-branch-only marker: %w", err)
	}
	if err := c.removeKeptPacks(); err != nil {
		return err
	}
	if len(remove) != 0 {
		input := "delete " + strings.Join(remove, "\ndelete ") + "\n"
		if _, err := workspace.CacheGitStdin(ctx, c.opts.Run, c.dir, input, "update-ref", "--no-deref", "--stdin"); err != nil {
			return fmt.Errorf("delete the cache's extra refs: %w", err)
		}
	}
	if err := c.runRepack(ctx); err != nil {
		outOfSpace := c.outOfSpace(err)
		c.removePartialPacks()
		err = fmt.Errorf("repack the narrowed cache: %w", err)
		if outOfSpace {
			return fmt.Errorf("%w: %w", errNarrowingOutOfSpace, err)
		}
		return err
	}
	after, err := c.objectBytes()
	if err != nil {
		return err
	}
	c.recordRepack(time.Now())
	if err := c.markDefaultBranchOnly(); err != nil {
		return err
	}
	c.opts.Log(fmt.Sprintf("narrowed %s %d %d", c.dir, before, after))
	return nil
}

func (c *cache) defaultBranchHead(ctx context.Context) (string, error) {
	head, err := workspace.CacheGit(ctx, c.opts.Run, c.dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("read the cache's default branch head: %w", err)
	}
	return strings.TrimSuffix(head.Stdout, "\n"), nil
}

// defaultBranchRewritten is whether the default branch's head moved from before to a commit that
// does not reach it: a force-push, or a default branch GitHub renamed to another history. `git
// rev-list before --not after` lists before when after does not reach it and nothing when it does,
// exiting 0 either way; `git merge-base --is-ancestor` tells the two apart only by its exit, which a
// checked run does not keep.
func (c *cache) defaultBranchRewritten(ctx context.Context, before string) (bool, error) {
	after, err := c.defaultBranchHead(ctx)
	if err != nil {
		return false, err
	}
	if before == after {
		return false, nil
	}
	unreached, err := workspace.CacheGit(ctx, c.opts.Run, c.dir, "rev-list", "--max-count=1", before, "--not", after)
	if err != nil {
		return false, fmt.Errorf("compare the cache's default branch heads: %w", err)
	}
	return strings.TrimSpace(unreached.Stdout) != "", nil
}

func (c *cache) objectBytes() (int64, error) {
	var bytes int64
	err := filepath.WalkDir(filepath.Join(c.dir, "objects"), func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		bytes += info.Size()
		return nil
	})
	return bytes, err
}

func (c *cache) marker(name string) (bool, error) {
	path := filepath.Join(c.opts.Root, name)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s %s is not a regular file", name, path)
	}
	return true, nil
}

func (c *cache) defaultBranchOnlyMarked() (bool, error) {
	return c.marker(defaultBranchOnlyFile)
}

func (c *cache) mark(name string) error {
	marker := filepath.Join(c.opts.Root, name)
	file, err := os.CreateTemp(c.opts.Root, name+".tmp-")
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(path, marker); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func (c *cache) markDefaultBranchOnly() error {
	return c.mark(defaultBranchOnlyFile)
}

func (c *cache) clear(name string) error {
	if err := os.Remove(filepath.Join(c.opts.Root, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

func (c *cache) removeKeptPacks() error {
	pack := filepath.Join(c.dir, "objects", "pack")
	for _, dir := range []string{filepath.Dir(pack), pack} {
		info, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("read %s for kept packs: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("read %s for kept packs: not a directory", dir)
		}
	}
	entries, err := os.ReadDir(pack)
	if err != nil {
		return fmt.Errorf("read %s for kept packs: %w", pack, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, "pack-") || !strings.HasSuffix(name, ".keep") {
			continue
		}
		if err := os.Remove(filepath.Join(pack, name)); err != nil {
			return fmt.Errorf("remove the kept pack %s: %w", name, err)
		}
	}
	return nil
}

// packs is the names of the finished packs (`pack-*.pack`) in the clone's objects/pack. The cache's
// fetch keeps what it brings as one pack (`fetch.unpackLimit=1`), and git writes that pack before it
// updates the default branch's ref. So a pack that appears across a fetch that failed can hold
// objects no ref reaches, which only a narrowing repack drops from what /tar streams.
func (c *cache) packs() (map[string]bool, error) {
	entries, err := os.ReadDir(filepath.Join(c.dir, "objects", "pack"))
	if err != nil {
		return nil, fmt.Errorf("read the cache clone's packs: %w", err)
	}
	packs := map[string]bool{}
	for _, entry := range entries {
		if name := entry.Name(); entry.Type().IsRegular() && strings.HasPrefix(name, "pack-") && strings.HasSuffix(name, ".pack") {
			packs[name] = true
		}
	}
	return packs, nil
}

// packAdded is whether after holds a pack that before does not.
func packAdded(before, after map[string]bool) bool {
	for name := range after {
		if !before[name] {
			return true
		}
	}
	return false
}
