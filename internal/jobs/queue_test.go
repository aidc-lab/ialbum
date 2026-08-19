package jobs

import (
	"context"
	"path/filepath"
	"testing"

	appdb "github.com/aidc-lab/ialbum/internal/db"
)

func TestListReturnsEmptyArrayShape(t *testing.T) {
	t.Parallel()
	database, err := appdb.Open(filepath.Join(t.TempDir(), "ialbum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	items, err := New(database, 1).List(context.Background(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if items == nil || len(items) != 0 {
		t.Fatalf("empty job list must be a non-nil slice: %#v", items)
	}
}
