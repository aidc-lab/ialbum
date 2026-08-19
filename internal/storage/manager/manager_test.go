package manager

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/aidc-lab/ialbum/internal/auth"
	appdb "github.com/aidc-lab/ialbum/internal/db"
)

func TestValidateSecretsRejectsWrongMasterKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	database, err := appdb.Open(filepath.Join(root, "ialbum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	first, _ := auth.NewSealer(bytes.Repeat([]byte{1}, 32))
	manager := NewManager(database, first)
	empty, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty list must be a non-nil slice: %#v", empty)
	}
	if _, err := manager.Create(context.Background(), CreateInput{Name: "local", Type: Local, Config: map[string]any{"root": root}}); err != nil {
		t.Fatal(err)
	}
	if err := manager.ValidateSecrets(context.Background()); err != nil {
		t.Fatal(err)
	}
	wrong, _ := auth.NewSealer(bytes.Repeat([]byte{2}, 32))
	if err := NewManager(database, wrong).ValidateSecrets(context.Background()); err == nil {
		t.Fatal("wrong master key should fail validation")
	}
}
