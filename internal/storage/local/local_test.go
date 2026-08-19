package local

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aidc-lab/ialbum/internal/storage"
)

func TestProviderContract(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	p, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	if _, err = p.Put(ctx, "家庭/旅行/你好.jpg", storage.BytesSource("abcdef"), storage.PutOptions{Conflict: "fail"}); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Put(ctx, "家庭/旅行/你好.jpg", storage.BytesSource("x"), storage.PutOptions{Conflict: "fail"}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	page, err := p.List(ctx, "家庭/旅行", "", 1)
	if err != nil || len(page.Objects) != 1 || page.Objects[0].RelativePath != "家庭/旅行/你好.jpg" {
		t.Fatalf("unexpected page: %+v, %v", page, err)
	}
	reader, object, err := p.Open(ctx, "家庭/旅行/你好.jpg", &storage.ByteRange{Start: 1, Length: 3})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(reader)
	_ = reader.Close()
	if string(raw) != "bcd" || object.Size != 6 {
		t.Fatalf("unexpected range %q object=%+v", raw, object)
	}
	if err := p.Move(ctx, "家庭/旅行/你好.jpg", "家庭/旅行/renamed.jpg", false); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, "家庭/旅行/renamed.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stat(ctx, "家庭/旅行/renamed.jpg"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestProviderRejectsEscapesAndSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.jpg"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Stat(context.Background(), "../secret.jpg"); err == nil {
		t.Fatal("path escape should fail")
	}
	if _, err := p.Stat(context.Background(), "escape/secret.jpg"); err == nil {
		t.Fatal("symlink escape should fail")
	}
}
