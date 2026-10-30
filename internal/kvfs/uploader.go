package kvfs

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"wpsdav/internal/logging"
	"wpsdav/internal/wps"
	"wpsdav/internal/wpsv7"
)

// UploadRequest describes one stored file.
type UploadRequest struct {
	GroupID  int64
	ParentID int64
	// ParentPath is the directory path relative to the drive root, used by the
	// v7 backend (which accepts names instead of folder ids).
	ParentPath []string
	Name       string
	Size       int64
	SHA1       string
	SHA256     string
	MD5        string
	// OverwriteFileID is the existing private file id for overwrite (private
	// backend). Overwrite is the same decision for the v7 backend.
	OverwriteFileID int64
	Overwrite       bool
}

// UploadResult reports the stored file. FileID may be zero when the backend
// succeeded but the private id could not be resolved yet.
type UploadResult struct {
	FileID int64
	Size   int64
	Mtime  int64
}

// Uploader stores file bytes.
type Uploader interface {
	Upload(ctx context.Context, req UploadRequest, body io.ReadSeeker) (UploadResult, error)
}

// PrivateUploader uses the cookie-authenticated netdisk API.
type PrivateUploader struct {
	Client *wps.Client
}

// Upload implements Uploader.
func (p *PrivateUploader) Upload(ctx context.Context, req UploadRequest, body io.ReadSeeker) (UploadResult, error) {
	resp, err := p.Client.Upload(ctx, wps.UploadOptions{
		GroupID:         req.GroupID,
		ParentID:        req.ParentID,
		Name:            req.Name,
		Hashes:          wps.Hashes{SHA1: req.SHA1, SHA256: req.SHA256, MD5: req.MD5, Size: req.Size},
		OverwriteFileID: req.OverwriteFileID,
	}, body)
	if err != nil {
		return UploadResult{}, err
	}
	return UploadResult{FileID: resp.ID, Size: req.Size}, nil
}

// PrivateLister is the lookup used to map a v7 upload back to a private id.
type PrivateLister interface {
	ListAll(ctx context.Context, groupID, parentID int64) ([]wps.FileInfo, error)
}

// V7Uploader uploads through the official v7 API and resolves the resulting
// private file id so downloads keep using the cookie backend.
type V7Uploader struct {
	Client *wpsv7.Client
	Lister PrivateLister
}

// Upload implements Uploader.
func (v *V7Uploader) Upload(ctx context.Context, req UploadRequest, body io.ReadSeeker) (UploadResult, error) {
	_, err := v.Client.Upload(ctx, wpsv7.UploadRequest{
		DriveID:    strconv.FormatInt(req.GroupID, 10),
		ParentPath: req.ParentPath,
		Name:       req.Name,
		Size:       req.Size,
		SHA256:     req.SHA256,
		MD5:        req.MD5,
		Overwrite:  req.Overwrite,
	}, body)
	if err != nil {
		return UploadResult{}, err
	}
	res := UploadResult{Size: req.Size}
	if v.Lister == nil {
		return res, nil
	}
	// The private listing usually sees the new file immediately, but the two
	// services are eventually consistent; retry briefly.
	for attempt := 0; attempt < 10; attempt++ {
		files, err := v.Lister.ListAll(ctx, req.GroupID, req.ParentID)
		if err == nil {
			for _, f := range files {
				if f.Name == req.Name && !f.IsDir() {
					res.FileID = f.ID
					res.Size = f.Size
					res.Mtime = f.Mtime
					return res, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return res, nil
		case <-time.After(300 * time.Millisecond):
		}
	}
	logging.Warnf("v7 upload succeeded but the private id for %s is not visible yet; cache will refresh on next access", req.Name)
	return res, nil
}

// FallbackUploader tries Primary, then Secondary. A successful primary (even
// with an unresolved private id) never falls back, avoiding duplicate files.
type FallbackUploader struct {
	Primary   Uploader
	Secondary Uploader
}

// Upload implements Uploader.
func (f *FallbackUploader) Upload(ctx context.Context, req UploadRequest, body io.ReadSeeker) (UploadResult, error) {
	res, err := f.Primary.Upload(ctx, req, body)
	if err == nil {
		return res, nil
	}
	if f.Secondary == nil {
		return res, err
	}
	logging.Warnf("v7 upload failed, falling back to private upload: %v", err)
	if _, seekErr := body.Seek(0, io.SeekStart); seekErr != nil {
		return res, fmt.Errorf("fallback upload: rewind body: %w", seekErr)
	}
	return f.Secondary.Upload(ctx, req, body)
}

// pathComponents turns "/a/b" into []string{"a","b"}.
func pathComponents(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" && part != "." && part != ".." {
			out = append(out, part)
		}
	}
	return out
}

// validateUploadPath rejects names that cannot be represented in a path.
func validateUploadPath(parts []string) error {
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, "/\\") {
			return fmt.Errorf("invalid upload path component %q", p)
		}
	}
	return nil
}
