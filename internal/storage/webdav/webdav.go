package webdav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aidc-lab/ialbum/internal/storage"
)

type Provider struct {
	base               *url.URL
	username, password string
	client             *http.Client
}
type multistatus struct {
	Responses []davResponse `xml:"response"`
}
type davResponse struct {
	Href  string     `xml:"href"`
	Props []propstat `xml:"propstat"`
}
type propstat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}
type davProp struct {
	Length       int64        `xml:"getcontentlength"`
	Modified     string       `xml:"getlastmodified"`
	ETag         string       `xml:"getetag"`
	ContentType  string       `xml:"getcontenttype"`
	ResourceType resourceType `xml:"resourcetype"`
}
type resourceType struct {
	Collection *struct{} `xml:"collection"`
}

func New(rawURL, root, username, password string) (*Provider, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("webdav URL must use http or https")
	}
	if u.Host == "" {
		return nil, errors.New("webdav URL requires a host")
	}
	rel, err := storage.NormalizeRelative(root)
	if err != nil {
		return nil, err
	}
	u.Path = path.Join(u.Path, "/"+rel) + "/"
	return &Provider{base: u, username: username, password: password, client: &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return errors.New("too many redirects")
		}
		if len(via) > 0 && req.URL.Host != via[0].URL.Host {
			return errors.New("cross-host redirect refused")
		}
		return nil
	}}}, nil
}
func (p *Provider) Capabilities() storage.Capabilities {
	return storage.Capabilities{Range: true, AtomicMove: true, NativeChecksum: true}
}
func (p *Provider) Validate(ctx context.Context) error { _, err := p.Stat(ctx, ""); return err }
func (p *Provider) List(ctx context.Context, dir, cursor string, limit int) (storage.Page, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:getcontentlength/><d:getlastmodified/><d:getetag/><d:getcontenttype/></d:prop></d:propfind>`
	resp, err := p.request(ctx, "PROPFIND", dir, strings.NewReader(body), map[string]string{"Depth": "1", "Content-Type": "application/xml"})
	if err != nil {
		return storage.Page{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 {
		return storage.Page{}, mapStatus(resp.StatusCode)
	}
	var result multistatus
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&result); err != nil {
		return storage.Page{}, err
	}
	offset, _ := strconv.Atoi(cursor)
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var objects []storage.Object
	basePath := p.base.Path
	for i, item := range result.Responses {
		if i == 0 {
			continue
		}
		if len(item.Props) == 0 {
			continue
		}
		prop := item.Props[0].Prop
		href, err := url.PathUnescape(item.Href)
		if err != nil {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(href, basePath), "/")
		rel = strings.TrimSuffix(rel, "/")
		if rel == "" {
			continue
		}
		modified, _ := http.ParseTime(prop.Modified)
		objects = append(objects, storage.Object{ID: item.Href, RelativePath: rel, MIMEType: prop.ContentType, ETag: strings.Trim(prop.ETag, "\""), NativeChecksum: strings.Trim(prop.ETag, "\""), Size: prop.Length, ModifiedAt: modified.UTC(), IsDir: prop.ResourceType.Collection != nil})
	}
	if offset > len(objects) {
		offset = len(objects)
	}
	end := offset + limit
	if end > len(objects) {
		end = len(objects)
	}
	page := storage.Page{Objects: objects[offset:end]}
	if end < len(objects) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}
func (p *Provider) Stat(ctx context.Context, name string) (storage.Object, error) {
	resp, err := p.request(ctx, "PROPFIND", name, nil, map[string]string{"Depth": "0"})
	if err != nil {
		return storage.Object{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 {
		return storage.Object{}, mapStatus(resp.StatusCode)
	}
	var result multistatus
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return storage.Object{}, err
	}
	if len(result.Responses) == 0 || len(result.Responses[0].Props) == 0 {
		return storage.Object{}, storage.ErrNotFound
	}
	prop := result.Responses[0].Props[0].Prop
	rel, _ := storage.NormalizeRelative(name)
	modified, _ := http.ParseTime(prop.Modified)
	return storage.Object{ID: result.Responses[0].Href, RelativePath: rel, MIMEType: prop.ContentType, ETag: strings.Trim(prop.ETag, "\""), NativeChecksum: strings.Trim(prop.ETag, "\""), Size: prop.Length, ModifiedAt: modified.UTC(), IsDir: prop.ResourceType.Collection != nil}, nil
}
func (p *Provider) Open(ctx context.Context, name string, br *storage.ByteRange) (io.ReadCloser, storage.Object, error) {
	obj, err := p.Stat(ctx, name)
	if err != nil {
		return nil, storage.Object{}, err
	}
	headers := map[string]string{}
	if br != nil {
		end := ""
		if br.Length > 0 {
			end = strconv.FormatInt(br.Start+br.Length-1, 10)
		}
		headers["Range"] = "bytes=" + strconv.FormatInt(br.Start, 10) + "-" + end
	}
	resp, err := p.request(ctx, http.MethodGet, name, nil, headers)
	if err != nil {
		return nil, storage.Object{}, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		return nil, storage.Object{}, mapStatus(resp.StatusCode)
	}
	obj.RangeApplied = br != nil && resp.StatusCode == http.StatusPartialContent
	return resp.Body, obj, nil
}
func (p *Provider) Put(ctx context.Context, name string, source storage.Source, opts storage.PutOptions) (storage.Object, error) {
	if opts.Conflict == "fail" {
		if _, err := p.Stat(ctx, name); err == nil {
			return storage.Object{}, storage.ErrConflict
		}
	}
	if err := p.Mkdir(ctx, path.Dir(name)); err != nil {
		return storage.Object{}, err
	}
	tmp := name + ".ialbum.tmp"
	in, err := source.Open(ctx)
	if err != nil {
		return storage.Object{}, err
	}
	defer in.Close()
	resp, err := p.request(ctx, http.MethodPut, tmp, in, map[string]string{"Content-Type": "application/octet-stream", "Content-Length": strconv.FormatInt(source.Size(), 10)})
	if err != nil {
		return storage.Object{}, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return storage.Object{}, mapStatus(resp.StatusCode)
	}
	if err := p.Move(ctx, tmp, name, true); err != nil {
		return storage.Object{}, err
	}
	return p.Stat(ctx, name)
}
func (p *Provider) Mkdir(ctx context.Context, name string) error {
	rel, err := storage.NormalizeRelative(name)
	if err != nil {
		return err
	}
	if rel == "" || rel == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(rel, "/") {
		current = path.Join(current, part)
		resp, err := p.request(ctx, "MKCOL", current, nil, nil)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 201 && resp.StatusCode != 200 && resp.StatusCode != 405 {
			return mapStatus(resp.StatusCode)
		}
	}
	return nil
}
func (p *Provider) Move(ctx context.Context, from, to string, overwrite bool) error {
	dest := p.objectURL(to).String()
	resp, err := p.request(ctx, "MOVE", from, nil, map[string]string{"Destination": dest, "Overwrite": map[bool]string{true: "T", false: "F"}[overwrite]})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapStatus(resp.StatusCode)
	}
	return nil
}
func (p *Provider) Delete(ctx context.Context, name string) error {
	resp, err := p.request(ctx, http.MethodDelete, name, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapStatus(resp.StatusCode)
	}
	return nil
}
func (p *Provider) request(ctx context.Context, method, name string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.objectURL(name).String(), body)
	if err != nil {
		return nil, err
	}
	if p.username != "" {
		req.SetBasicAuth(p.username, p.password)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if raw := req.Header.Get("Content-Length"); raw != "" {
		if size, err := strconv.ParseInt(raw, 10, 64); err == nil {
			req.ContentLength = size
			req.Header.Del("Content-Length")
		}
	}
	return p.client.Do(req)
}
func (p *Provider) objectURL(name string) *url.URL {
	rel, _ := storage.NormalizeRelative(name)
	u := *p.base
	u.Path = path.Join(p.base.Path, rel)
	if rel == "" {
		u.Path = strings.TrimSuffix(p.base.Path, "/")
	}
	return &u
}
func mapStatus(code int) error {
	switch code {
	case 401, 403:
		return storage.ErrUnauthorized
	case 404:
		return storage.ErrNotFound
	case 409, 412:
		return storage.ErrConflict
	case 429:
		return storage.ErrRateLimited
	case 500, 502, 503, 504:
		return storage.ErrTransient
	default:
		return fmt.Errorf("webdav returned HTTP %d", code)
	}
}
