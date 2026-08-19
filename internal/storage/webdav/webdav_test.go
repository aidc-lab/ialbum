package webdav

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aidc-lab/ialbum/internal/storage"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestWebDAVListAndRangeFallback(t *testing.T) {
	p, err := New("https://dav.example.test/root", "相册", "user", "password")
	if err != nil {
		t.Fatal(err)
	}
	var getRange string
	p.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		username, password, ok := r.BasicAuth()
		if !ok || username != "user" || password != "password" {
			t.Error("missing Basic authentication")
		}
		switch r.Method {
		case "PROPFIND":
			body := `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>/root/%E7%9B%B8%E5%86%8C/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat></d:response><d:response><d:href>/root/%E7%9B%B8%E5%86%8C/photo.jpg</d:href><d:propstat><d:prop><d:getcontentlength>6</d:getcontentlength><d:getetag>"etag"</d:getetag><d:getcontenttype>image/jpeg</d:getcontenttype><d:resourcetype/></d:prop></d:propstat></d:response></d:multistatus>`
			return response(207, body), nil
		case http.MethodGet:
			getRange = r.Header.Get("Range")
			return response(http.StatusOK, "abcdef"), nil
		default:
			return response(http.StatusNoContent, ""), nil
		}
	})}
	page, err := p.List(context.Background(), "", "", 10)
	if err != nil || len(page.Objects) != 1 || page.Objects[0].RelativePath != "photo.jpg" {
		t.Fatalf("unexpected page: %+v, %v", page, err)
	}
	reader, object, err := p.Open(context.Background(), "photo.jpg", &storage.ByteRange{Start: 1, Length: 2})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(reader)
	_ = reader.Close()
	if getRange != "bytes=1-2" || object.RangeApplied || string(raw) != "abcdef" {
		t.Fatalf("range=%q object=%+v body=%q", getRange, object, raw)
	}
}

func TestWebDAVPutSetsKnownLength(t *testing.T) {
	p, err := New("https://dav.example.test", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case "PROPFIND":
			if strings.HasSuffix(r.URL.Path, "photo.jpg") {
				return response(207, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"><d:response><d:href>/photo.jpg</d:href><d:propstat><d:prop><d:getcontentlength>3</d:getcontentlength><d:resourcetype/></d:prop></d:propstat></d:response></d:multistatus>`), nil
			}
			return response(http.StatusNotFound, ""), nil
		case http.MethodPut:
			if r.ContentLength != 3 {
				t.Errorf("ContentLength=%d", r.ContentLength)
			}
			return response(http.StatusCreated, ""), nil
		case "MOVE":
			return response(http.StatusCreated, ""), nil
		default:
			return response(http.StatusMethodNotAllowed, ""), nil
		}
	})}
	if _, err := p.Put(context.Background(), "photo.jpg", storage.BytesSource("abc"), storage.PutOptions{Conflict: "overwrite"}); err != nil {
		t.Fatal(err)
	}
}
