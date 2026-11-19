package media_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mino/internal/media"
	"mino/internal/testutil"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func setup(t *testing.T, maxSize int64) (*media.Service, *media.Local, int64, int64) {
	t.Helper()
	pool := testutil.DB(t)
	store := &media.Local{Dir: t.TempDir(), BaseURL: "/uploads"}
	svc := media.NewService(pool, store, maxSize, testutil.Logger())
	return svc, store, testutil.Site(t, pool, "a"), testutil.Site(t, pool, "b")
}

func TestUploadImage(t *testing.T) {
	svc, store, site, _ := setup(t, 1<<20)
	ctx := context.Background()
	f, err := svc.Upload(ctx, site, 0, "../../etc/Photo.PNG", bytes.NewReader(pngBytes(t, 40, 30)))
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "Photo.PNG" || f.Mime != "image/png" || f.Width == nil || *f.Width != 40 || *f.Height != 30 {
		t.Fatalf("metadata: %+v", f)
	}
	if !strings.HasPrefix(f.Key, "1/") || !strings.HasSuffix(f.Key, ".png") || !strings.HasPrefix(f.URL, "/uploads/1/") {
		t.Fatalf("key/url: %s %s", f.Key, f.URL)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, filepath.FromSlash(f.Key))); err != nil {
		t.Fatalf("file not stored: %v", err)
	}
}

func TestUploadValidation(t *testing.T) {
	svc, _, site, _ := setup(t, 1024)
	ctx := context.Background()
	cases := map[string][]byte{
		"evil.svg":  []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"page.html": []byte(`<!DOCTYPE html><html><script>alert(1)</script></html>`),
		"bin.exe":   {0x4d, 0x5a, 0x90, 0x00, 0x03, 0x00, 0x00, 0x00, 0xff, 0xfe, 0x00, 0x01},
		"empty.txt": {},
	}
	for name, body := range cases {
		if _, err := svc.Upload(ctx, site, 0, name, bytes.NewReader(body)); !errors.Is(err, media.ErrType) {
			t.Errorf("%s: want ErrType, got %v", name, err)
		}
	}
	if _, err := svc.Upload(ctx, site, 0, "big.txt", bytes.NewReader(bytes.Repeat([]byte("a"), 1025))); !errors.Is(err, media.ErrTooLarge) {
		t.Errorf("want ErrTooLarge, got %v", err)
	}
	f, err := svc.Upload(ctx, site, 0, "data.csv", strings.NewReader("a,b\n1,2\n"))
	if err != nil || f.Mime != "text/csv" {
		t.Fatalf("csv: %v %+v", err, f)
	}
	f, err = svc.Upload(ctx, site, 0, "doc.pdf", strings.NewReader("%PDF-1.4\n%..."))
	if err != nil || f.Mime != "application/pdf" {
		t.Fatalf("pdf: %v %+v", err, f)
	}
}

func TestLibraryOperations(t *testing.T) {
	svc, store, siteA, siteB := setup(t, 1<<20)
	ctx := context.Background()
	img, _ := svc.Upload(ctx, siteA, 0, "beach.png", bytes.NewReader(pngBytes(t, 2, 2)))
	doc, _ := svc.Upload(ctx, siteA, 0, "notes.txt", strings.NewReader("hello"))
	other, _ := svc.Upload(ctx, siteB, 0, "beach.png", bytes.NewReader(pngBytes(t, 2, 2)))

	list, total, err := svc.List(ctx, siteA, media.ListQuery{})
	if err != nil || total != 2 || len(list) != 2 {
		t.Fatalf("list: %v total=%d", err, total)
	}
	if _, total, _ = svc.List(ctx, siteA, media.ListQuery{Category: "images"}); total != 1 {
		t.Fatalf("images filter: %d", total)
	}
	if _, total, _ = svc.List(ctx, siteA, media.ListQuery{Category: "files"}); total != 1 {
		t.Fatalf("files filter: %d", total)
	}
	if _, err := svc.Update(ctx, siteA, img.ID, "Beach", "Sunset over the sea", "Summer"); err != nil {
		t.Fatal(err)
	}
	if _, total, _ = svc.List(ctx, siteA, media.ListQuery{Search: "sunset"}); total != 1 {
		t.Fatalf("search on alt text: %d", total)
	}

	// Sites cannot see or change each other's files.
	if _, err := svc.Get(ctx, siteA, other.ID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("cross-site get: %v", err)
	}
	if err := svc.Delete(ctx, siteA, other.ID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("cross-site delete: %v", err)
	}

	if err := svc.Delete(ctx, siteA, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, filepath.FromSlash(doc.Key))); !os.IsNotExist(err) {
		t.Fatal("stored file not removed")
	}

	if err := svc.DeleteSiteFiles(ctx, siteB); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, filepath.FromSlash(other.Key))); !os.IsNotExist(err) {
		t.Fatal("site files not removed")
	}
	if _, err := os.Stat(filepath.Join(store.Dir, filepath.FromSlash(img.Key))); err != nil {
		t.Fatal("other site's files removed")
	}
}

func TestLocalRejectsTraversal(t *testing.T) {
	l := &media.Local{Dir: t.TempDir()}
	if err := l.Put(context.Background(), "../x", strings.NewReader("x"), 1, "", ""); err == nil {
		t.Fatal("traversal key accepted")
	}
}
