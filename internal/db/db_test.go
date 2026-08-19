package db

import (
	"path/filepath"
	"testing"
)

func TestOpenRunsMigrationsAndEnablesPragmas(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "ialbum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('users','albums','jobs','media_items')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("expected migrated tables, got %d", count)
	}
	var foreignKeys int
	if err := database.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys=%d", foreignKeys)
	}
}
