package kvfs

import (
	"context"
	"io"
	"testing"
	"time"

	"wpsdav/internal/wps"
)

type fakeLister struct {
	items map[int64][]wps.FileInfo
	calls int
}

func (f *fakeLister) ListAll(_ context.Context, _, parentID int64) ([]wps.FileInfo, error) {
	f.calls++
	return f.items[parentID], nil
}

// TestSeedRootVisibleToStat guards against the root cache key mismatch
// ("" vs "/") that made freshly created folders invisible to Stat.
func TestSeedRootVisibleToStat(t *testing.T) {
	f := &fakeLister{items: map[int64][]wps.FileInfo{}}
	tree := NewTree(f, 1, time.Minute)

	tree.Seed("/", &Node{Name: "WPS-DAV", ID: 42, GroupID: 1, IsDir: true})
	n, err := tree.Stat(context.Background(), "/WPS-DAV")
	if err != nil {
		t.Fatalf("Stat seeded root child: %v", err)
	}
	if n.ID != 42 {
		t.Fatalf("wrong node: %+v", n)
	}

	f.items[42] = []wps.FileInfo{
		{Name: "a.txt", ID: 7, GroupID: 1, ParentID: 42, Size: 5, Type: "file"},
	}
	children, err := tree.List(context.Background(), "/WPS-DAV")
	if err != nil {
		t.Fatalf("List seeded dir: %v", err)
	}
	var got *Node
	for _, child := range children {
		if child.Name == "a.txt" {
			got = child
		}
	}
	if got == nil || got.ID != 7 {
		t.Fatalf("missing child: %+v", children)
	}

	nested, err := tree.Stat(context.Background(), "/WPS-DAV/a.txt")
	if err != nil || nested.ID != 7 {
		t.Fatalf("nested Stat: %v %+v", err, nested)
	}
}

// TestDirFileReaddirEmpty ensures an empty directory returns an empty slice
// first, then io.EOF — the contract x/net/webdav's walkFS relies on.
func TestDirFileReaddirEmpty(t *testing.T) {
	f := &fakeLister{items: map[int64][]wps.FileInfo{}}
	tree := NewTree(f, 1, time.Minute)
	tree.Seed("/", &Node{Name: "empty", ID: 5, GroupID: 1, IsDir: true})
	d := &dirFile{
		fs:   &FS{Tree: tree},
		ctx:  context.Background(),
		path: "/empty",
		node: &Node{Name: "empty", ID: 5, GroupID: 1, IsDir: true},
	}
	infos, err := d.Readdir(0)
	if err != nil {
		t.Fatalf("first Readdir: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("expected no entries, got %d", len(infos))
	}
	if _, err := d.Readdir(0); err != io.EOF {
		t.Fatalf("second Readdir = %v, want io.EOF", err)
	}
}

// TestJanitorEvictsExpired guards the long-run memory bound: expired
// directory entries must actually be released.
func TestJanitorEvictsExpired(t *testing.T) {
	f := &fakeLister{items: map[int64][]wps.FileInfo{}}
	tree := NewTree(f, 1, 10*time.Millisecond)
	tree.Seed("/", &Node{Name: "a", ID: 1, GroupID: 1, IsDir: true})

	time.Sleep(30 * time.Millisecond)
	tree.evictExpired(time.Now())

	tree.mu.Lock()
	n := len(tree.dirs)
	tree.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected expired entries to be evicted, still have %d", n)
	}
}
