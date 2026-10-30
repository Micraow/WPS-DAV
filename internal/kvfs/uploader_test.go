package kvfs

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeUploader struct {
	err    error
	id     int64
	called int
}

func (f *fakeUploader) Upload(_ context.Context, _ UploadRequest, _ io.ReadSeeker) (UploadResult, error) {
	f.called++
	return UploadResult{FileID: f.id}, f.err
}

func TestFallbackUploader(t *testing.T) {
	bad := &fakeUploader{err: errors.New("v7 down")}
	good := &fakeUploader{id: 42}
	fb := &FallbackUploader{Primary: bad, Secondary: good}

	res, err := fb.Upload(context.Background(), UploadRequest{Name: "a.txt"}, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if res.FileID != 42 || good.called != 1 || bad.called != 1 {
		t.Fatalf("res=%+v bad=%d good=%d", res, bad.called, good.called)
	}
}

func TestFallbackUploaderNoFallbackOnSuccess(t *testing.T) {
	ok := &fakeUploader{id: 7}
	alt := &fakeUploader{id: 8}
	fb := &FallbackUploader{Primary: ok, Secondary: alt}
	res, err := fb.Upload(context.Background(), UploadRequest{Name: "a.txt"}, strings.NewReader("x"))
	if err != nil || res.FileID != 7 || alt.called != 0 {
		t.Fatalf("res=%+v err=%v alt=%d", res, err, alt.called)
	}
}

func TestPathComponents(t *testing.T) {
	if got := pathComponents("/"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if got := pathComponents("/a/b"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("%v", got)
	}
}
