package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aidc-lab/ialbum/internal/storage"
)

type Provider struct {
	rootPath string
	root     *os.Root
}

func New(rootPath string) (*Provider, error) {
	if !filepath.IsAbs(rootPath) {
		return nil, errors.New("local storage root must be absolute")
	}
	info, err := os.Stat(rootPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("local storage root is not a directory")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	return &Provider{rootPath: filepath.Clean(rootPath), root: root}, nil
}
func (p *Provider) Close() error                   { return p.root.Close() }
func (p *Provider) Validate(context.Context) error { _, err := p.root.Stat("."); return err }
func (p *Provider) Capabilities() storage.Capabilities {
	return storage.Capabilities{Range: true, AtomicMove: true, NativeChecksum: false, Multipart: false, StableObjectID: false}
}

func (p *Provider) List(ctx context.Context, dir, cursor string, limit int) (storage.Page, error) {
	rel, err := storage.NormalizeRelative(dir)
	if err != nil {
		return storage.Page{}, err
	}
	if rel == "" {
		rel = "."
	}
	entries, err := fs.ReadDir(p.root.FS(), rel)
	if err != nil {
		return storage.Page{}, mapError(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	offset, _ := strconv.Atoi(cursor)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	end := offset + limit
	if end > len(entries) {
		end = len(entries)
	}
	page := storage.Page{}
	for _, entry := range entries[offset:end] {
		select {
		case <-ctx.Done():
			return storage.Page{}, ctx.Err()
		default:
		}
		child := entry.Name()
		if rel != "." {
			child = filepath.ToSlash(filepath.Join(rel, entry.Name()))
		}
		info, err := entry.Info()
		if err != nil {
			return storage.Page{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		page.Objects = append(page.Objects, fromInfo(child, info))
	}
	if end < len(entries) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}
func (p *Provider) Stat(_ context.Context, name string) (storage.Object, error) {
	rel, err := clean(name)
	if err != nil {
		return storage.Object{}, err
	}
	info, err := p.root.Lstat(rel)
	if err != nil {
		return storage.Object{}, mapError(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return storage.Object{}, storage.ErrUnsupported
	}
	return fromInfo(rel, info), nil
}
func (p *Provider) Open(ctx context.Context, name string, br *storage.ByteRange) (io.ReadCloser, storage.Object, error) {
	obj, err := p.Stat(ctx, name)
	if err != nil {
		return nil, storage.Object{}, err
	}
	if obj.IsDir {
		return nil, storage.Object{}, storage.ErrUnsupported
	}
	rel, _ := clean(name)
	file, err := p.root.Open(rel)
	if err != nil {
		return nil, storage.Object{}, mapError(err)
	}
	if br == nil {
		return file, obj, nil
	}
	if br.Start < 0 || br.Start >= obj.Size {
		file.Close()
		return nil, storage.Object{}, fmt.Errorf("invalid range")
	}
	length := br.Length
	if length <= 0 || br.Start+length > obj.Size {
		length = obj.Size - br.Start
	}
	obj.RangeApplied = true
	return &sectionReadCloser{Reader: io.NewSectionReader(file, br.Start, length), closer: file}, obj, nil
}
func (p *Provider) Put(ctx context.Context, name string, source storage.Source, opts storage.PutOptions) (storage.Object, error) {
	rel, err := clean(name)
	if err != nil {
		return storage.Object{}, err
	}
	if rel == "." {
		return storage.Object{}, storage.ErrConflict
	}
	if err := p.ensureParents(rel); err != nil {
		return storage.Object{}, err
	}
	if opts.Conflict == "fail" {
		if _, err := p.root.Stat(rel); err == nil {
			return storage.Object{}, storage.ErrConflict
		}
	}
	tmp := rel + ".ialbum-" + randomSuffix() + ".tmp"
	out, err := p.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return storage.Object{}, err
	}
	in, err := source.Open(ctx)
	if err != nil {
		out.Close()
		_ = p.root.Remove(tmp)
		return storage.Object{}, err
	}
	_, copyErr := copyContext(ctx, out, in)
	closeErr := out.Close()
	_ = in.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = p.root.Remove(tmp)
		return storage.Object{}, copyErr
	}
	if err := p.safeRename(tmp, rel); err != nil {
		_ = p.root.Remove(tmp)
		return storage.Object{}, err
	}
	return p.Stat(ctx, rel)
}
func (p *Provider) Mkdir(_ context.Context, name string) error {
	rel, err := clean(name)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	return p.ensureDir(rel)
}
func (p *Provider) Move(_ context.Context, from, to string, overwrite bool) error {
	src, err := clean(from)
	if err != nil {
		return err
	}
	dst, err := clean(to)
	if err != nil {
		return err
	}
	if err := p.ensureParents(dst); err != nil {
		return err
	}
	if !overwrite {
		if _, err := p.root.Stat(dst); err == nil {
			return storage.ErrConflict
		}
	}
	return p.safeRename(src, dst)
}
func (p *Provider) Delete(_ context.Context, name string) error {
	rel, err := clean(name)
	if err != nil {
		return err
	}
	if rel == "." {
		return storage.ErrUnsupported
	}
	if err := p.root.Remove(rel); err != nil {
		return mapError(err)
	}
	return nil
}

func (p *Provider) ensureParents(rel string) error {
	parent := filepath.ToSlash(filepath.Dir(rel))
	if parent == "." {
		return nil
	}
	return p.ensureDir(parent)
}
func (p *Provider) ensureDir(rel string) error {
	current := ""
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" {
			continue
		}
		if current == "" {
			current = part
		} else {
			current += "/" + part
		}
		info, err := p.root.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return storage.ErrConflict
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := p.root.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return nil
}
func (p *Provider) safeRename(from, to string) error {
	for _, rel := range []string{from, to} {
		parent := filepath.Dir(rel)
		if parent != "." {
			info, err := p.root.Lstat(parent)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return storage.ErrConflict
			}
		}
	}
	return os.Rename(filepath.Join(p.rootPath, filepath.FromSlash(from)), filepath.Join(p.rootPath, filepath.FromSlash(to)))
}
func clean(value string) (string, error) {
	rel, err := storage.NormalizeRelative(value)
	if rel == "" {
		rel = "."
	}
	return rel, err
}
func fromInfo(rel string, info fs.FileInfo) storage.Object {
	return storage.Object{ID: rel, RelativePath: filepath.ToSlash(strings.TrimPrefix(rel, "./")), MIMEType: storage.MIMEFromName(rel), Size: info.Size(), ModifiedAt: info.ModTime().UTC(), IsDir: info.IsDir(), ETag: fmt.Sprintf("%x-%x", info.ModTime().UnixNano(), info.Size())}
}
func mapError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return storage.ErrNotFound
	}
	if errors.Is(err, os.ErrPermission) {
		return storage.ErrUnauthorized
	}
	return err
}
func randomSuffix() string {
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}
func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 64*1024)
	var total int64
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		n, er := src.Read(buf)
		if n > 0 {
			wn, ew := dst.Write(buf[:n])
			total += int64(wn)
			if ew != nil {
				return total, ew
			}
			if wn != n {
				return total, io.ErrShortWrite
			}
		}
		if er == io.EOF {
			return total, nil
		}
		if er != nil {
			return total, er
		}
	}
}

type sectionReadCloser struct {
	io.Reader
	closer io.Closer
}

func (s *sectionReadCloser) Close() error { return s.closer.Close() }

var _ = time.Time{}
