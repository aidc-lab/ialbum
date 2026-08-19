package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"strings"
	"time"
)

var (
	ErrNotFound     = errors.New("storage object not found")
	ErrConflict     = errors.New("storage object conflict")
	ErrUnauthorized = errors.New("storage authentication failed")
	ErrRateLimited  = errors.New("storage rate limited")
	ErrUnsupported  = errors.New("storage operation unsupported")
	ErrTransient    = errors.New("temporary storage error")
)

type Capabilities struct{ Range, AtomicMove, NativeChecksum, Multipart, StableObjectID bool }
type ByteRange struct{ Start, Length int64 }
type Object struct {
	ID, RelativePath, MIMEType, ETag, NativeChecksum string
	Size                                             int64
	ModifiedAt                                       time.Time
	IsDir, RangeApplied                              bool
}
type Page struct {
	Objects    []Object
	NextCursor string
}
type PutOptions struct{ Conflict string }

type Source interface {
	Open(context.Context) (io.ReadCloser, error)
	Size() int64
}

type Provider interface {
	Validate(context.Context) error
	Capabilities() Capabilities
	List(context.Context, string, string, int) (Page, error)
	Stat(context.Context, string) (Object, error)
	Open(context.Context, string, *ByteRange) (io.ReadCloser, Object, error)
	Put(context.Context, string, Source, PutOptions) (Object, error)
	Mkdir(context.Context, string) error
	Move(context.Context, string, string, bool) error
	Delete(context.Context, string) error
}

type FileSource struct {
	Path     string
	ByteSize int64
}

func (f FileSource) Open(_ context.Context) (io.ReadCloser, error) { return os.Open(f.Path) }
func (f FileSource) Size() int64                                   { return f.ByteSize }

type BytesSource []byte

func (b BytesSource) Open(_ context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(string(b))), nil
}
func (b BytesSource) Size() int64 { return int64(len(b)) }

func NormalizeRelative(value string) (string, error) {
	if strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("path contains NUL")
	}
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	if value == "" || value == "." {
		return "", nil
	}
	if strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("absolute path is not allowed")
	}
	cleaned := path.Clean(value)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path escapes storage root")
	}
	if cleaned == "." {
		return "", nil
	}
	return strings.TrimPrefix(cleaned, "./"), nil
}

func MIMEFromName(name string) string {
	if value := mime.TypeByExtension(strings.ToLower(path.Ext(name))); value != "" {
		return strings.Split(value, ";")[0]
	}
	return "application/octet-stream"
}
