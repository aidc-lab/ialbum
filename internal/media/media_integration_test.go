package media

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aidc-lab/ialbum/internal/auth"
	appdb "github.com/aidc-lab/ialbum/internal/db"
	"github.com/aidc-lab/ialbum/internal/jobs"
	storagemanager "github.com/aidc-lab/ialbum/internal/storage/manager"
)

func TestLocalAlbumUploadThumbnailAndBackup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	primaryDir := filepath.Join(root, "primary")
	backupDir := filepath.Join(root, "backup")
	cacheDir := filepath.Join(root, "cache")
	tempDir := filepath.Join(root, "tmp")
	for _, dir := range []string{primaryDir, backupDir, cacheDir, tempDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	database, err := appdb.Open(filepath.Join(root, "ialbum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sealer, err := auth.NewSealer(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	manager := storagemanager.NewManager(database, sealer)
	primary, err := manager.Create(ctx, storagemanager.CreateInput{Name: "primary", Type: storagemanager.Local, Config: map[string]any{"root": primaryDir}})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := manager.Create(ctx, storagemanager.CreateInput{Name: "backup", Type: storagemanager.Local, Config: map[string]any{"root": backupDir}})
	if err != nil {
		t.Fatal(err)
	}
	queue := jobs.New(database, 1)
	service := NewService(database, manager, queue, tempDir, cacheDir, time.Hour)
	emptyAlbums, err := service.ListAlbums(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if emptyAlbums == nil || len(emptyAlbums) != 0 {
		t.Fatalf("empty album list must be []: %#v", emptyAlbums)
	}
	interval := int64(3600)
	album, err := service.CreateAlbum(ctx, CreateAlbumInput{Name: "旅行", PrimaryStorageID: primary.ID, PrimaryPath: "photos", BackupStorageID: backup.ID, BackupPath: "copies", BackupMode: "safe", BackupEnabled: true, SyncOnUpload: true, ScanIntervalSeconds: &interval})
	if err != nil {
		t.Fatal(err)
	}

	fixture := filepath.Join(root, "fixture.jpg")
	file, err := os.Create(fixture)
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 80, 60))
	for y := 0; y < 60; y++ {
		for x := 0; x < 80; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 3), G: uint8(y * 4), B: 90, A: 255})
		}
	}
	if err := jpeg.Encode(file, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(fixture)
	item, err := service.Upload(ctx, album.ID, "海边.jpg", fixture, "upload-1", info.Size())
	if err != nil {
		t.Fatal(err)
	}

	processor := NewProcessor(database, service, cacheDir, tempDir, "", "", 2<<30)
	if err := processor.Process(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	processed, err := service.GetMedia(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if processed.Width == nil || *processed.Width != 80 || processed.Height == nil || *processed.Height != 60 || processed.ProcessingStatus != "ready" {
		t.Fatalf("unexpected processed media: %+v", processed)
	}
	thumb, _, err := processor.OpenThumbnail(ctx, item.ID, "grid")
	if err != nil {
		t.Fatal(err)
	}
	thumbRaw, _ := io.ReadAll(thumb)
	_ = thumb.Close()
	if len(thumbRaw) == 0 {
		t.Fatal("empty thumbnail")
	}

	if err := service.Backup(ctx, album.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	backedUp, err := os.ReadFile(filepath.Join(backupDir, "copies", "海边.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(fixture)
	if !bytes.Equal(backedUp, original) {
		t.Fatal("backup differs from source")
	}

	again, err := service.Upload(ctx, album.ID, "ignored.jpg", fixture, "upload-1", info.Size())
	if err != nil || again.ID != item.ID {
		t.Fatalf("idempotent upload returned %+v, %v", again, err)
	}
}
