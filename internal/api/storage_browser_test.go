package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aidc-lab/ialbum/internal/auth"
	appdb "github.com/aidc-lab/ialbum/internal/db"
	storagemanager "github.com/aidc-lab/ialbum/internal/storage/manager"
)

func TestStorageBrowserListsAndStreamsFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	storageRoot := filepath.Join(root, "storage")
	if err := os.MkdirAll(filepath.Join(storageRoot, "旅行"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(storageRoot, "旅行", "第一天"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storageRoot, "旅行", "照片.jpg"), []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := appdb.Open(filepath.Join(root, "ialbum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sealer, _ := auth.NewSealer(bytes.Repeat([]byte{4}, 32))
	manager := storagemanager.NewManager(database, sealer)
	connection, err := manager.Create(context.Background(), storagemanager.CreateInput{Name: "照片盘", Type: storagemanager.Local, Config: map[string]any{"root": storageRoot}})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{storages: manager}

	browseRequest := withStorageID(httptest.NewRequest(http.MethodGet, "/api/v1/storage-connections/id/objects?path="+url.QueryEscape("旅行"), nil), connection.ID)
	browseResponse := httptest.NewRecorder()
	server.handleBrowseStorage(browseResponse, browseRequest)
	if browseResponse.Code != http.StatusOK {
		t.Fatalf("browse status=%d body=%s", browseResponse.Code, browseResponse.Body.String())
	}
	var envelope struct {
		Data struct {
			CurrentPath string                `json:"currentPath"`
			Items       []storageBrowserEntry `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(browseResponse.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.CurrentPath != "旅行" || len(envelope.Data.Items) != 2 || !envelope.Data.Items[0].IsDir || envelope.Data.Items[0].Name != "第一天" || envelope.Data.Items[1].Name != "照片.jpg" {
		t.Fatalf("unexpected browse response: %+v", envelope.Data)
	}

	contentRequest := withStorageID(httptest.NewRequest(http.MethodGet, "/api/v1/storage-connections/id/content?path="+url.QueryEscape("旅行/照片.jpg"), nil), connection.ID)
	contentRequest.Header.Set("Range", "bytes=1-3")
	contentResponse := httptest.NewRecorder()
	server.handleStorageObjectContent(contentResponse, contentRequest)
	if contentResponse.Code != http.StatusPartialContent || contentResponse.Body.String() != "bcd" || contentResponse.Header().Get("Content-Range") != "bytes 1-3/6" {
		t.Fatalf("unexpected content response: status=%d range=%q body=%q", contentResponse.Code, contentResponse.Header().Get("Content-Range"), contentResponse.Body.String())
	}
}

func TestStorageBrowserRejectsTraversal(t *testing.T) {
	server := &Server{}
	request := withStorageID(httptest.NewRequest(http.MethodGet, "/api/v1/storage-connections/id/objects?path=../secret", nil), "id")
	response := httptest.NewRecorder()
	server.handleBrowseStorage(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func withStorageID(request *http.Request, id string) *http.Request {
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("id", id)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}
