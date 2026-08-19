package baidu

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aidc-lab/ialbum/internal/storage"
)

const blockSize = int64(4 << 20)

type Token struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
}
type Config struct {
	AppKey, SecretKey, Root       string
	Token                         Token
	SaveToken                     func(context.Context, Token) error
	OpenAPIBase, PanBase, PCSBase string
}
type Provider struct {
	cfg    Config
	client *http.Client
	mu     sync.Mutex
}

type DeviceAuthorization struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	QRCodeURL       string `json:"qrcode_url"`
	ExpiresIn       int64  `json:"expires_in"`
	Interval        int64  `json:"interval"`
}
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func defaults(cfg Config) Config {
	if cfg.OpenAPIBase == "" {
		cfg.OpenAPIBase = "https://openapi.baidu.com"
	}
	if cfg.PanBase == "" {
		cfg.PanBase = "https://pan.baidu.com"
	}
	if cfg.PCSBase == "" {
		cfg.PCSBase = "https://d.pcs.baidu.com"
	}
	return cfg
}
func New(cfg Config) *Provider {
	return &Provider{cfg: defaults(cfg), client: &http.Client{Timeout: 30 * time.Minute}}
}

func StartDeviceAuthorization(ctx context.Context, client *http.Client, base, appKey string) (DeviceAuthorization, error) {
	if base == "" {
		base = "https://openapi.baidu.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	endpoint := strings.TrimSuffix(base, "/") + "/oauth/2.0/device/code?" + url.Values{"response_type": {"device_code"}, "client_id": {appKey}, "scope": {"basic netdisk"}}.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	resp, err := client.Do(req)
	if err != nil {
		return DeviceAuthorization{}, err
	}
	defer resp.Body.Close()
	var result DeviceAuthorization
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return result, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || result.DeviceCode == "" {
		return result, fmt.Errorf("baidu device authorization failed: HTTP %d", resp.StatusCode)
	}
	if result.Interval < 3 {
		result.Interval = 5
	}
	return result, nil
}
func PollDeviceAuthorization(ctx context.Context, client *http.Client, base, appKey, secretKey, deviceCode string) (Token, string, error) {
	if base == "" {
		base = "https://openapi.baidu.com"
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	values := url.Values{"grant_type": {"device_token"}, "code": {deviceCode}, "client_id": {appKey}, "client_secret": {secretKey}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+"/oauth/2.0/token", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return Token{}, "", err
	}
	defer resp.Body.Close()
	var result tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return Token{}, "", err
	}
	if result.Error != "" {
		return Token{}, result.Error, errors.New(result.ErrorDescription)
	}
	if result.AccessToken == "" {
		return Token{}, "invalid_response", errors.New("baidu token response has no access token")
	}
	return Token{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, ExpiresAt: time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)}, "", nil
}

func (p *Provider) Capabilities() storage.Capabilities {
	return storage.Capabilities{Range: true, NativeChecksum: true, Multipart: true, StableObjectID: true}
}
func (p *Provider) Validate(ctx context.Context) error { _, err := p.List(ctx, "", "", 1); return err }
func (p *Provider) List(ctx context.Context, dir, cursor string, limit int) (storage.Page, error) {
	token, err := p.accessToken(ctx)
	if err != nil {
		return storage.Page{}, err
	}
	rel, err := storage.NormalizeRelative(dir)
	if err != nil {
		return storage.Page{}, err
	}
	remote := p.remotePath(rel)
	start, _ := strconv.Atoi(cursor)
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	values := url.Values{"method": {"list"}, "access_token": {token}, "dir": {remote}, "start": {strconv.Itoa(start)}, "limit": {strconv.Itoa(limit)}, "web": {"0"}}
	var result struct {
		Errno int `json:"errno"`
		List  []struct {
			FsID     int64  `json:"fs_id"`
			Path     string `json:"path"`
			Name     string `json:"server_filename"`
			Size     int64  `json:"size"`
			IsDir    int    `json:"isdir"`
			MTime    int64  `json:"server_mtime"`
			MD5      string `json:"md5"`
			Category int    `json:"category"`
		} `json:"list"`
	}
	if err := p.jsonRequest(ctx, http.MethodGet, p.cfg.PanBase+"/rest/2.0/xpan/file?"+values.Encode(), nil, &result); err != nil {
		return storage.Page{}, err
	}
	if result.Errno != 0 {
		return storage.Page{}, mapErrno(result.Errno)
	}
	page := storage.Page{}
	for _, item := range result.List {
		itemRel := strings.TrimPrefix(item.Path, strings.TrimSuffix(p.remotePath(""), "/")+"/")
		mime := storage.MIMEFromName(item.Name)
		page.Objects = append(page.Objects, storage.Object{ID: strconv.FormatInt(item.FsID, 10), RelativePath: itemRel, MIMEType: mime, Size: item.Size, ModifiedAt: time.Unix(item.MTime, 0).UTC(), IsDir: item.IsDir == 1, NativeChecksum: item.MD5, ETag: item.MD5})
	}
	if len(result.List) == limit {
		page.NextCursor = strconv.Itoa(start + limit)
	}
	return page, nil
}
func (p *Provider) Stat(ctx context.Context, name string) (storage.Object, error) {
	rel, err := storage.NormalizeRelative(name)
	if err != nil {
		return storage.Object{}, err
	}
	dir := path.Dir(rel)
	if dir == "." {
		dir = ""
	}
	cursor := ""
	for {
		page, err := p.List(ctx, dir, cursor, 200)
		if err != nil {
			return storage.Object{}, err
		}
		for _, obj := range page.Objects {
			if obj.RelativePath == rel {
				return obj, nil
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return storage.Object{}, storage.ErrNotFound
}
func (p *Provider) Open(ctx context.Context, name string, br *storage.ByteRange) (io.ReadCloser, storage.Object, error) {
	obj, err := p.Stat(ctx, name)
	if err != nil {
		return nil, storage.Object{}, err
	}
	fsID, err := strconv.ParseInt(obj.ID, 10, 64)
	if err != nil {
		return nil, storage.Object{}, err
	}
	token, err := p.accessToken(ctx)
	if err != nil {
		return nil, storage.Object{}, err
	}
	fsids, _ := json.Marshal([]int64{fsID})
	values := url.Values{"method": {"filemetas"}, "access_token": {token}, "fsids": {string(fsids)}, "dlink": {"1"}}
	var meta struct {
		Errno int `json:"errno"`
		List  []struct {
			DLink string `json:"dlink"`
		} `json:"list"`
	}
	if err := p.jsonRequest(ctx, http.MethodGet, p.cfg.PanBase+"/rest/2.0/xpan/multimedia?"+values.Encode(), nil, &meta); err != nil {
		return nil, storage.Object{}, err
	}
	if meta.Errno != 0 || len(meta.List) == 0 {
		return nil, storage.Object{}, mapErrno(meta.Errno)
	}
	dlink := meta.List[0].DLink
	separator := "?"
	if strings.Contains(dlink, "?") {
		separator = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlink+separator+"access_token="+url.QueryEscape(token), nil)
	if err != nil {
		return nil, storage.Object{}, err
	}
	req.Header.Set("User-Agent", "pan.baidu.com")
	if br != nil {
		end := ""
		if br.Length > 0 {
			end = strconv.FormatInt(br.Start+br.Length-1, 10)
		}
		req.Header.Set("Range", "bytes="+strconv.FormatInt(br.Start, 10)+"-"+end)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, storage.Object{}, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		return nil, storage.Object{}, mapHTTP(resp.StatusCode)
	}
	obj.RangeApplied = br != nil && resp.StatusCode == http.StatusPartialContent
	return resp.Body, obj, nil
}
func (p *Provider) Put(ctx context.Context, name string, source storage.Source, opts storage.PutOptions) (storage.Object, error) {
	rel, err := storage.NormalizeRelative(name)
	if err != nil {
		return storage.Object{}, err
	}
	if err := p.Mkdir(ctx, path.Dir(rel)); err != nil {
		return storage.Object{}, err
	}
	hashes, err := blockHashes(ctx, source)
	if err != nil {
		return storage.Object{}, err
	}
	token, err := p.accessToken(ctx)
	if err != nil {
		return storage.Object{}, err
	}
	blockJSON, _ := json.Marshal(hashes)
	rtype := "0"
	if opts.Conflict == "rename" {
		rtype = "1"
	}
	form := url.Values{"path": {p.remotePath(rel)}, "size": {strconv.FormatInt(source.Size(), 10)}, "isdir": {"0"}, "autoinit": {"1"}, "block_list": {string(blockJSON)}, "rtype": {rtype}}
	var pre struct {
		Errno    int    `json:"errno"`
		UploadID string `json:"uploadid"`
		Blocks   []int  `json:"block_list"`
	}
	if err := p.formJSON(ctx, p.cfg.PanBase+"/rest/2.0/xpan/file?method=precreate&access_token="+url.QueryEscape(token), form, &pre); err != nil {
		return storage.Object{}, err
	}
	if pre.Errno != 0 {
		return storage.Object{}, mapErrno(pre.Errno)
	}
	needed := map[int]bool{}
	for _, n := range pre.Blocks {
		needed[n] = true
	}
	if len(needed) == 0 {
		for i := range hashes {
			needed[i] = true
		}
	}
	in, err := source.Open(ctx)
	if err != nil {
		return storage.Object{}, err
	}
	defer in.Close()
	for i := range hashes {
		chunk, readErr := io.ReadAll(io.LimitReader(in, blockSize))
		if readErr != nil {
			return storage.Object{}, readErr
		}
		if !needed[i] {
			continue
		}
		if err := p.uploadBlock(ctx, token, p.remotePath(rel), pre.UploadID, i, chunk); err != nil {
			return storage.Object{}, err
		}
	}
	create := url.Values{"path": {p.remotePath(rel)}, "size": {strconv.FormatInt(source.Size(), 10)}, "isdir": {"0"}, "uploadid": {pre.UploadID}, "block_list": {string(blockJSON)}, "rtype": {rtype}}
	var completed struct {
		Errno int    `json:"errno"`
		FsID  int64  `json:"fs_id"`
		Path  string `json:"path"`
		Size  int64  `json:"size"`
		MD5   string `json:"md5"`
		MTime int64  `json:"server_mtime"`
	}
	if err := p.formJSON(ctx, p.cfg.PanBase+"/rest/2.0/xpan/file?method=create&access_token="+url.QueryEscape(token), create, &completed); err != nil {
		return storage.Object{}, err
	}
	if completed.Errno != 0 {
		return storage.Object{}, mapErrno(completed.Errno)
	}
	return storage.Object{ID: strconv.FormatInt(completed.FsID, 10), RelativePath: rel, MIMEType: storage.MIMEFromName(rel), Size: completed.Size, ModifiedAt: time.Unix(completed.MTime, 0).UTC(), ETag: completed.MD5, NativeChecksum: completed.MD5}, nil
}
func (p *Provider) Mkdir(ctx context.Context, name string) error {
	rel, err := storage.NormalizeRelative(name)
	if err != nil {
		return err
	}
	if rel == "" || rel == "." {
		return nil
	}
	token, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	form := url.Values{"path": {p.remotePath(rel)}, "isdir": {"1"}, "rtype": {"0"}}
	var result struct {
		Errno int `json:"errno"`
	}
	if err := p.formJSON(ctx, p.cfg.PanBase+"/rest/2.0/xpan/file?method=create&access_token="+url.QueryEscape(token), form, &result); err != nil {
		return err
	}
	if result.Errno == 0 || result.Errno == -8 {
		return nil
	}
	return mapErrno(result.Errno)
}
func (p *Provider) Move(ctx context.Context, from, to string, overwrite bool) error {
	return p.fileManager(ctx, "move", from, to, overwrite)
}
func (p *Provider) Delete(ctx context.Context, name string) error {
	token, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	filelist, _ := json.Marshal([]string{p.remotePath(name)})
	form := url.Values{"async": {"0"}, "filelist": {string(filelist)}}
	var result struct {
		Errno int `json:"errno"`
	}
	if err := p.formJSON(ctx, p.cfg.PanBase+"/rest/2.0/xpan/file?method=filemanager&opera=delete&access_token="+url.QueryEscape(token), form, &result); err != nil {
		return err
	}
	if result.Errno != 0 {
		return mapErrno(result.Errno)
	}
	return nil
}

func (p *Provider) fileManager(ctx context.Context, operation, from, to string, overwrite bool) error {
	token, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	toRel, err := storage.NormalizeRelative(to)
	if err != nil {
		return err
	}
	item := []map[string]string{{"path": p.remotePath(from), "dest": path.Dir(p.remotePath(toRel)), "newname": path.Base(toRel)}}
	filelist, _ := json.Marshal(item)
	ondup := "fail"
	if overwrite {
		ondup = "overwrite"
	}
	form := url.Values{"async": {"0"}, "ondup": {ondup}, "filelist": {string(filelist)}}
	var result struct {
		Errno int `json:"errno"`
	}
	if err := p.formJSON(ctx, p.cfg.PanBase+"/rest/2.0/xpan/file?method=filemanager&opera="+operation+"&access_token="+url.QueryEscape(token), form, &result); err != nil {
		return err
	}
	if result.Errno != 0 {
		return mapErrno(result.Errno)
	}
	return nil
}
func (p *Provider) uploadBlock(ctx context.Context, token, remotePath, uploadID string, index int, data []byte) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "block")
	if err != nil {
		return err
	}
	if _, err = part.Write(data); err != nil {
		return err
	}
	_ = writer.Close()
	values := url.Values{"method": {"upload"}, "type": {"tmpfile"}, "access_token": {token}, "path": {remotePath}, "uploadid": {uploadID}, "partseq": {strconv.Itoa(index)}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.PCSBase+"/rest/2.0/pcs/superfile2?"+values.Encode(), &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapHTTP(resp.StatusCode)
	}
	var result struct {
		MD5       string `json:"md5"`
		ErrorCode int    `json:"error_code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return err
	}
	if result.ErrorCode != 0 {
		return mapErrno(result.ErrorCode)
	}
	return nil
}
func (p *Provider) accessToken(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cfg.Token.AccessToken != "" && time.Now().Add(2*time.Minute).Before(p.cfg.Token.ExpiresAt) {
		return p.cfg.Token.AccessToken, nil
	}
	if p.cfg.Token.RefreshToken == "" {
		return "", storage.ErrUnauthorized
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {p.cfg.Token.RefreshToken}, "client_id": {p.cfg.AppKey}, "client_secret": {p.cfg.SecretKey}}
	var result tokenResponse
	if err := p.formJSON(ctx, p.cfg.OpenAPIBase+"/oauth/2.0/token", form, &result); err != nil {
		return "", err
	}
	if result.Error != "" || result.AccessToken == "" {
		return "", storage.ErrUnauthorized
	}
	token := Token{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, ExpiresAt: time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)}
	if token.RefreshToken == "" {
		token.RefreshToken = p.cfg.Token.RefreshToken
	}
	if p.cfg.SaveToken != nil {
		if err := p.cfg.SaveToken(ctx, token); err != nil {
			return "", err
		}
	}
	p.cfg.Token = token
	return token.AccessToken, nil
}
func (p *Provider) remotePath(rel string) string {
	root := "/" + strings.Trim(p.cfg.Root, "/")
	if root == "/" {
		root = ""
	}
	rel, _ = storage.NormalizeRelative(rel)
	return path.Join("/", root, rel)
}
func (p *Provider) jsonRequest(ctx context.Context, method, endpoint string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapHTTP(resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}
func (p *Provider) formJSON(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapHTTP(resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}
func blockHashes(ctx context.Context, source storage.Source) ([]string, error) {
	in, err := source.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	var hashes []string
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		h := md5.New()
		n, err := io.CopyN(h, in, blockSize)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, err
		}
		if n > 0 {
			hashes = append(hashes, hex.EncodeToString(h.Sum(nil)))
		}
		if n < blockSize {
			break
		}
	}
	if len(hashes) == 0 {
		sum := md5.Sum(nil)
		hashes = []string{hex.EncodeToString(sum[:])}
	}
	return hashes, nil
}
func mapHTTP(code int) error {
	switch code {
	case 401, 403:
		return storage.ErrUnauthorized
	case 404:
		return storage.ErrNotFound
	case 409:
		return storage.ErrConflict
	case 429:
		return storage.ErrRateLimited
	case 500, 502, 503, 504:
		return storage.ErrTransient
	default:
		return fmt.Errorf("baidu returned HTTP %d", code)
	}
}
func mapErrno(errno int) error {
	switch errno {
	case 0:
		return nil
	case -6:
		return storage.ErrUnauthorized
	case -8:
		return storage.ErrConflict
	case -9:
		return storage.ErrNotFound
	case 31034:
		return storage.ErrRateLimited
	default:
		return fmt.Errorf("baidu errno %d", errno)
	}
}
