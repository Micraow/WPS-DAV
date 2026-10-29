// Package kvfs adapts the WPS API to a WebDAV filesystem with a
// path→node cache that rides out WPS listing eventual consistency.
package kvfs

import (
	"context"
	"errors"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"wpsdav/internal/wps"
)

// Node is one cached filesystem entry.
type Node struct {
	Name     string
	ID       int64 // file id; 0 for the root pseudo-node
	GroupID  int64
	ParentID int64
	Size     int64
	Mtime    time.Time
	IsDir    bool
}

type dirEntry struct {
	children map[string]*Node
	expires  time.Time
}

type inflightCall struct {
	done     chan struct{}
	children map[string]*Node
	err      error
}

// Lister is the subset of the WPS client the cache needs.
type Lister interface {
	ListAll(ctx context.Context, groupID, parentID int64) ([]wps.FileInfo, error)
}

// Tree is the cached view of one WPS group.
type Tree struct {
	client   Lister
	rootID   int64
	ttl      time.Duration
	mu       sync.Mutex
	dirs     map[string]*dirEntry
	inflight map[string]*inflightCall
	nowFunc  func() time.Time
}

// NewTree builds a cache rooted at the given group.
func NewTree(client Lister, rootGroupID int64, ttl time.Duration) *Tree {
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &Tree{
		client:   client,
		rootID:   rootGroupID,
		ttl:      ttl,
		dirs:     make(map[string]*dirEntry),
		inflight: make(map[string]*inflightCall),
		nowFunc:  time.Now,
	}
}

func clean(p string) string {
	c := path.Clean("/" + p)
	return c
}

// Root returns the root pseudo-node.
func (t *Tree) Root() *Node {
	return &Node{Name: "/", GroupID: t.rootID, ID: 0, IsDir: true}
}

// Stat resolves a path to a node.
func (t *Tree) Stat(ctx context.Context, p string) (*Node, error) {
	p = clean(p)
	if p == "/" {
		return t.Root(), nil
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	cur := t.Root()
	curPath := "/"
	for _, part := range parts {
		if !cur.IsDir {
			return nil, os.ErrNotExist
		}
		children, err := t.children(ctx, curPath, cur)
		if err != nil {
			return nil, err
		}
		next, ok := children[part]
		if !ok {
			return nil, os.ErrNotExist
		}
		cur = next
		curPath = path.Join(curPath, part)
	}
	return cur, nil
}

// List returns the children of a directory path.
func (t *Tree) List(ctx context.Context, p string) (map[string]*Node, error) {
	p = clean(p)
	n, err := t.Stat(ctx, p)
	if err != nil {
		return nil, err
	}
	if !n.IsDir {
		return nil, os.ErrInvalid
	}
	return t.children(ctx, p, n)
}

// children returns the (possibly cached) listing of dir. Concurrent misses
// for the same directory are collapsed into one upstream request.
func (t *Tree) children(ctx context.Context, dirPath string, dir *Node) (map[string]*Node, error) {
	t.mu.Lock()
	if de, ok := t.dirs[dirPath]; ok && t.nowFunc().Before(de.expires) {
		children := de.children // snapshot: never mutated in place
		t.mu.Unlock()
		return children, nil
	}
	if call, ok := t.inflight[dirPath]; ok {
		t.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return nil, call.err
			}
			return call.children, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &inflightCall{done: make(chan struct{})}
	t.inflight[dirPath] = call
	t.mu.Unlock()

	files, err := t.client.ListAll(ctx, dir.GroupID, dir.ID)
	var children map[string]*Node
	if err == nil {
		children = make(map[string]*Node, len(files))
		for _, f := range files {
			children[f.Name] = &Node{
				Name:     f.Name,
				ID:       f.ID,
				GroupID:  f.GroupID,
				ParentID: f.ParentID,
				Size:     f.Size,
				Mtime:    time.Unix(f.Mtime, 0),
				IsDir:    f.IsDir(),
			}
		}
	}

	t.mu.Lock()
	delete(t.inflight, dirPath)
	if err != nil {
		// Serve stale data rather than failing the whole mount on a blip.
		if de, ok := t.dirs[dirPath]; ok {
			children = de.children
			err = nil
		}
	} else {
		t.dirs[dirPath] = &dirEntry{children: children, expires: t.nowFunc().Add(t.ttl)}
	}
	call.children, call.err = children, err
	close(call.done)
	t.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return children, nil
}

// Seed inserts a node into a directory's cache directly. This hides the
// eventual-consistency lag of the WPS children endpoint after writes.
// Uses copy-on-write so readers holding a snapshot are never raced.
func (t *Tree) Seed(dirPath string, n *Node) {
	dirPath = clean(dirPath)
	t.mu.Lock()
	defer t.mu.Unlock()
	de, ok := t.dirs[dirPath]
	children := make(map[string]*Node)
	expires := t.nowFunc().Add(2 * t.ttl)
	if ok {
		for k, v := range de.children {
			children[k] = v
		}
		if de.expires.After(t.nowFunc()) {
			expires = de.expires
		}
	}
	children[n.Name] = n
	t.dirs[dirPath] = &dirEntry{children: children, expires: expires}
}

// Forget drops the cached listing for dirPath (and any descendants).
func (t *Tree) Forget(dirPath string) {
	dirPath = clean(dirPath)
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.dirs, dirPath)
	prefix := dirPath
	if prefix != "/" {
		prefix += "/"
	}
	for p := range t.dirs {
		if p != dirPath && strings.HasPrefix(p, prefix) {
			delete(t.dirs, p)
		}
	}
}

// Remove deletes a name from its parent's cached listing without forgetting
// the whole directory (used after deletes/renames).
func (t *Tree) Remove(dirPath, name string) {
	dirPath = clean(dirPath)
	t.mu.Lock()
	defer t.mu.Unlock()
	de, ok := t.dirs[dirPath]
	if !ok {
		return
	}
	children := make(map[string]*Node, len(de.children))
	for k, v := range de.children {
		if k != name {
			children[k] = v
		}
	}
	de.children = children
}

// ForcedRefresh clears all caches.
func (t *Tree) ForcedRefresh() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dirs = make(map[string]*dirEntry)
}

// Janitor periodically drops expired directory entries. Without it, every
// directory ever browsed would stay resident for the life of the process.
func (t *Tree) Janitor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.evictExpired(t.nowFunc())
		}
	}
}

func (t *Tree) evictExpired(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for p, de := range t.dirs {
		if de.expires.Before(now) {
			delete(t.dirs, p)
		}
	}
}

// ErrReadOnly is returned when a mutating operation is disabled.
var ErrReadOnly = errors.New("read-only mode")
