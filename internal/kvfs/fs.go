package kvfs

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"wpsdav/internal/logging"
	"wpsdav/internal/ui"
	"wpsdav/internal/wps"
)

// FS implements webdav.FileSystem on top of one WPS group.
type FS struct {
	Tree   *Tree
	Client *wps.Client
	// ReadOnly blocks mkdir/put/delete/move/copy.
	ReadOnly bool
	// SpoolDir is where PUT bodies are buffered for hashing.
	SpoolDir string
}

var _ webdav.FileSystem = (*FS)(nil)

// Mkdir creates a directory.
func (fs *FS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if fs.ReadOnly {
		return ErrReadOnly
	}
	name = clean(name)
	if name == "/" {
		return os.ErrExist
	}
	parentPath, base := path.Split(name)
	parentPath = clean(parentPath)
	parent, err := fs.Tree.Stat(ctx, parentPath)
	if err != nil {
		return err
	}
	if _, err := fs.Tree.Stat(ctx, name); err == nil {
		return os.ErrExist
	}
	if err := ValidName(base); err != nil {
		return err
	}
	id, err := fs.Client.Mkdir(ctx, parent.GroupID, parent.ID, base)
	if err != nil {
		return err
	}
	fs.Tree.Seed(parentPath, &Node{
		Name: base, ID: id, GroupID: parent.GroupID, ParentID: parent.ID,
		IsDir: true, Mtime: time.Now(),
	})
	return nil
}

// OpenFile opens a file for read or write. Reads of regular files are served
// by the HTTP layer (redirect/proxy); directory opens serve PROPFIND.
func (fs *FS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	name = clean(name)
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) == 0 {
		n, err := fs.Tree.Stat(ctx, name)
		if err != nil {
			return nil, err
		}
		if n.IsDir {
			return &dirFile{fs: fs, ctx: ctx, path: name, node: n}, nil
		}
		return &fileReader{fs: fs, path: name, node: n}, nil
	}
	if fs.ReadOnly {
		return nil, ErrReadOnly
	}
	parentPath, base := path.Split(name)
	parentPath = clean(parentPath)
	parent, err := fs.Tree.Stat(ctx, parentPath)
	if err != nil {
		return nil, err
	}
	if !parent.IsDir {
		return nil, os.ErrInvalid
	}
	if err := ValidName(base); err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp(fs.SpoolDir, "wpsdav-upload-*")
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}

	uf := &uploadFile{
		fs:      fs,
		ctx:     ctx,
		dirPath: parentPath,
		parent:  parent,
		name:    base,
		tmp:     tmp,
		sha1:    sha1.New(),
		sha256:  sha256.New(),
		md5:     md5.New(),
		start:   time.Now(),
	}
	// Detect an existing target so the upload replaces it instead of
	// letting WPS mint a "name(1)" duplicate.
	if existing, err := fs.Tree.Stat(ctx, name); err == nil && !existing.IsDir {
		uf.overwriteID = existing.ID
	}
	return uf, nil
}

// RemoveAll deletes a file or directory tree.
func (fs *FS) RemoveAll(ctx context.Context, name string) error {
	if fs.ReadOnly {
		return ErrReadOnly
	}
	name = clean(name)
	if name == "/" {
		return os.ErrPermission
	}
	n, err := fs.Tree.Stat(ctx, name)
	if err != nil {
		return err
	}
	if err := fs.Client.Delete(ctx, n.GroupID, n.ID); err != nil {
		return err
	}
	parentPath, base := path.Split(name)
	fs.Tree.Remove(clean(parentPath), base)
	fs.Tree.Forget(name)
	return nil
}

// Stat returns file info for a path.
func (fs *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	n, err := fs.Tree.Stat(ctx, name)
	if err != nil {
		return nil, err
	}
	return nodeInfo{n}, nil
}

// Rename implements MOVE (rename within a folder or move across folders).
func (fs *FS) Rename(ctx context.Context, oldName, newName string) error {
	if fs.ReadOnly {
		return ErrReadOnly
	}
	oldName, newName = clean(oldName), clean(newName)
	if oldName == "/" || newName == "/" {
		return os.ErrPermission
	}
	src, err := fs.Tree.Stat(ctx, oldName)
	if err != nil {
		return err
	}
	newParentPath, newBase := path.Split(newName)
	newParentPath = clean(newParentPath)
	dstParent, err := fs.Tree.Stat(ctx, newParentPath)
	if err != nil {
		return err
	}
	if !dstParent.IsDir {
		return os.ErrInvalid
	}

	if src.ParentID == dstParent.ID && dstParent.GroupID == src.GroupID {
		if err := fs.Client.Rename(ctx, src.GroupID, src.ID, newBase); err != nil {
			return err
		}
	} else {
		if err := fs.Client.Move(ctx, src.GroupID, src.ID, dstParent.GroupID, dstParent.ID); err != nil {
			return err
		}
		// The move API keeps the original basename; rename if the target differs.
		if src.Name != newBase {
			if err := fs.Client.Rename(ctx, dstParent.GroupID, src.ID, newBase); err != nil {
				fs.Tree.Forget(oldName)
				fs.Tree.Forget(newParentPath)
				return err
			}
		}
	}

	oldParentPath, oldBase := path.Split(oldName)
	fs.Tree.Remove(clean(oldParentPath), oldBase)
	moved := *src
	moved.Name = newBase
	moved.ParentID = dstParent.ID
	moved.GroupID = dstParent.GroupID
	fs.Tree.Seed(newParentPath, &moved)
	fs.Tree.Forget(newName)
	return nil
}

// Copy implements COPY.
func (fs *FS) Copy(ctx context.Context, oldName, newName string, overwrite bool) error {
	if fs.ReadOnly {
		return ErrReadOnly
	}
	oldName, newName = clean(oldName), clean(newName)
	src, err := fs.Tree.Stat(ctx, oldName)
	if err != nil {
		return err
	}
	dstParentPath, dstBase := path.Split(newName)
	dstParentPath = clean(dstParentPath)
	dstParent, err := fs.Tree.Stat(ctx, dstParentPath)
	if err != nil {
		return err
	}
	if !dstParent.IsDir {
		return os.ErrInvalid
	}

	if existing, err := fs.Tree.Stat(ctx, newName); err == nil {
		if !overwrite {
			return os.ErrExist
		}
		if err := fs.Client.Delete(ctx, existing.GroupID, existing.ID); err != nil {
			return err
		}
		fs.Tree.Remove(dstParentPath, dstBase)
		fs.Tree.Forget(newName)
	}

	if err := fs.Client.Copy(ctx, src.GroupID, src.ID, dstParent.GroupID, dstParent.ID); err != nil {
		return err
	}
	// WPS keeps the source name on copy; rename if the target differs. The
	// copy is asynchronous server-side, so poll the listing briefly for the
	// new entry before giving up.
	srcBase := path.Base(oldName)
	if srcBase != dstBase {
		fs.Tree.Forget(dstParentPath)
		var copied *Node
		for attempt := 0; attempt < 4; attempt++ {
			children, err := fs.Tree.List(ctx, dstParentPath)
			if err == nil {
				if n, ok := children[srcBase]; ok {
					copied = n
					break
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(400 * time.Millisecond):
			}
			fs.Tree.Forget(dstParentPath)
		}
		if copied == nil {
			return fmt.Errorf("copy: new entry %q did not appear in listing", srcBase)
		}
		if err := fs.Client.Rename(ctx, copied.GroupID, copied.ID, dstBase); err != nil {
			return err
		}
		fs.Tree.Remove(dstParentPath, srcBase)
		copied.Name = dstBase
		fs.Tree.Seed(dstParentPath, copied)
	}
	return nil
}

// nodeInfo adapts a Node to os.FileInfo.
type nodeInfo struct{ n *Node }

func (fi nodeInfo) Name() string { return fi.n.Name }
func (fi nodeInfo) Size() int64 {
	if fi.n.IsDir {
		return 0
	}
	return fi.n.Size
}
func (fi nodeInfo) Mode() os.FileMode {
	if fi.n.IsDir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
func (fi nodeInfo) ModTime() time.Time { return fi.n.Mtime }
func (fi nodeInfo) IsDir() bool        { return fi.n.IsDir }
func (fi nodeInfo) Sys() any           { return nil }

// ContentType implements webdav.ContentTyper so PROPFIND never has to open
// the file (reads are served directly by the HTTP layer).
func (fi nodeInfo) ContentType(context.Context) (string, error) {
	if fi.n.IsDir {
		return "httpd/unix-directory", nil
	}
	if ct := mime.TypeByExtension(filepath.Ext(fi.n.Name)); ct != "" {
		return ct, nil
	}
	return "application/octet-stream", nil
}

// uploadFile buffers a PUT body, then pushes it through the WPS
// three-stage upload on Close.
type uploadFile struct {
	fs                *FS
	ctx               context.Context
	dirPath           string
	parent            *Node
	name              string
	tmp               *os.File
	sha1, sha256, md5 hash.Hash
	written           int64
	overwriteID       int64
	start             time.Time
	closed            bool
}

func (u *uploadFile) Write(p []byte) (int, error) {
	n, err := u.tmp.Write(p)
	if n > 0 {
		_, _ = u.sha1.Write(p[:n])
		_, _ = u.sha256.Write(p[:n])
		_, _ = u.md5.Write(p[:n])
		u.written += int64(n)
	}
	return n, err
}

func (u *uploadFile) Close() error {
	if u.closed {
		return nil
	}
	u.closed = true
	defer func() {
		name := u.tmp.Name()
		u.tmp.Close()
		os.Remove(name)
	}()

	if _, err := u.tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hashes := wps.Hashes{
		SHA1:   hex.EncodeToString(u.sha1.Sum(nil)),
		SHA256: hex.EncodeToString(u.sha256.Sum(nil)),
		MD5:    hex.EncodeToString(u.md5.Sum(nil)),
		Size:   u.written,
	}
	resp, err := u.fs.Client.Upload(u.ctx, wps.UploadOptions{
		GroupID:         u.parent.GroupID,
		ParentID:        u.parent.ID,
		Name:            u.name,
		Hashes:          hashes,
		OverwriteFileID: u.overwriteID,
	}, u.tmp)
	fullPath := path.Join(u.dirPath, u.name)
	if err != nil {
		logging.Errorf("%s upload failed: %s: %v", ui.Tag("upload"), fullPath, err)
		return err
	}
	logging.Transfer("upload", fullPath, hashes.Size, time.Since(u.start))
	newID := resp.ID
	if newID == 0 {
		newID = u.overwriteID
	}
	u.fs.Tree.Seed(u.dirPath, &Node{
		Name:     u.name,
		ID:       newID,
		GroupID:  u.parent.GroupID,
		ParentID: u.parent.ID,
		Size:     hashes.Size,
		Mtime:    time.Now(),
	})
	return nil
}

func (u *uploadFile) Read([]byte) (int, error) { return 0, os.ErrInvalid }
func (u *uploadFile) Seek(int64, int) (int64, error) {
	return 0, os.ErrInvalid
}
func (u *uploadFile) Readdir(int) ([]os.FileInfo, error) {
	return nil, os.ErrInvalid
}
func (u *uploadFile) Stat() (os.FileInfo, error) {
	return nodeInfo{&Node{
		Name: u.name, ID: u.overwriteID, GroupID: u.parent.GroupID,
		ParentID: u.parent.ID, Size: u.written, Mtime: time.Now(),
	}}, nil
}

// Suffix helper kept private: WPS rejects names starting with a dot.
func ValidName(name string) error {
	if name == "" || name == "." || name == ".." {
		return os.ErrInvalid
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("wps: names starting with a dot are rejected by the server: %w", os.ErrInvalid)
	}
	if strings.ContainsAny(name, "/\\") {
		return os.ErrInvalid
	}
	return nil
}

// dirFile serves directory reads for PROPFIND depth-1.
type dirFile struct {
	fs   *FS
	ctx  context.Context
	path string
	node *Node
	off  int
	done bool
}

func (d *dirFile) Close() error                   { return nil }
func (d *dirFile) Read([]byte) (int, error)       { return 0, os.ErrInvalid }
func (d *dirFile) Seek(int64, int) (int64, error) { return 0, os.ErrInvalid }
func (d *dirFile) Write([]byte) (int, error)      { return 0, os.ErrInvalid }
func (d *dirFile) Stat() (os.FileInfo, error)     { return nodeInfo{d.node}, nil }

func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	children, err := d.fs.Tree.List(d.ctx, d.path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(children))
	for name := range children {
		names = append(names, name)
	}
	sort.Strings(names)

	if count <= 0 {
		// "all entries": the first call returns the full slice (possibly
		// empty) with a nil error; the next call reports io.EOF.
		if d.done {
			return nil, io.EOF
		}
		d.done = true
		d.off = len(names)
		out := make([]os.FileInfo, 0, len(names))
		for _, name := range names {
			out = append(out, nodeInfo{children[name]})
		}
		return out, nil
	}

	if d.off >= len(names) {
		return nil, io.EOF
	}
	end := len(names)
	if d.off+count < end {
		end = d.off + count
	}
	out := make([]os.FileInfo, 0, end-d.off)
	for _, name := range names[d.off:end] {
		out = append(out, nodeInfo{children[name]})
	}
	d.off = end
	return out, nil
}

// fileReader is a read handle for a regular file. The WebDAV GET/HEAD
// requests are answered earlier by the HTTP layer, so this is only a
// safety net; it streams through the presigned URL if ever used.
type fileReader struct {
	fs   *FS
	path string
	node *Node
}

func (f *fileReader) Close() error                       { return nil }
func (f *fileReader) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }
func (f *fileReader) Write([]byte) (int, error)          { return 0, os.ErrInvalid }
func (f *fileReader) Stat() (os.FileInfo, error)         { return nodeInfo{f.node}, nil }
func (f *fileReader) Seek(int64, int) (int64, error)     { return 0, os.ErrInvalid }
func (f *fileReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("kvfs: file reads are served by the HTTP layer")
}
