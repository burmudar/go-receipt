package remotefs

import (
	"context"
	"crypto/sha256"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/studio-b12/gowebdav"
)

type Dir struct {
	client *gowebdav.Client
}

type File struct {
	Path string
	Data []byte
}

func (r *File) Name() string {
	name := filepath.Base(r.Path)
	return name
}

func (r *File) Content() []byte {
	return r.Data
}

func (r *File) SHA() [32]byte {
	return sha256.Sum256(r.Data)
}

func (r *File) Ext() string {
	return filepath.Ext(r.Path)
}

func NewDir(user, pass, endpoint string) (*Dir, error) {
	return NewDirContext(context.Background(), user, pass, endpoint)
}

// NewDirContext confines every request, including authentication, to the configured
// endpoint. Redirects are disabled, and cancellation applies to every operation.
func NewDirContext(ctx context.Context, user, pass, endpoint string) (*Dir, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid WebDAV endpoint")
	}
	base := strings.TrimSuffix(u.Path, "/")
	if strings.Contains(base, "\\") || (base != "" && path.Clean(base) != base) {
		return nil, fmt.Errorf("invalid WebDAV endpoint path")
	}
	client := gowebdav.NewClient(endpoint, user, pass)
	client.SetTimeout(30 * time.Second)
	client.SetHeader(gowebdav.XInhibitRedirect, "1")
	client.SetTransport(endpointTransport{base: u, path: base, next: http.DefaultTransport})
	client.SetInterceptor(func(_ string, req *http.Request) { *req = *req.WithContext(ctx) })

	if err := client.Connect(); err != nil {
		return nil, err
	}

	return &Dir{
		client: client,
	}, nil
}

type endpointTransport struct {
	base *url.URL
	path string
	next http.RoundTripper
}

func (t endpointTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p := r.URL.Path
	if r.URL.Scheme != t.base.Scheme || r.URL.Host != t.base.Host || r.URL.User != nil ||
		strings.Contains(p, "\\") || (p != "/" && path.Clean(p) != strings.TrimSuffix(p, "/")) ||
		(t.path != "" && p != t.path && !strings.HasPrefix(p, t.path+"/")) {
		return nil, fmt.Errorf("WebDAV request outside configured endpoint")
	}
	return t.next.RoundTrip(r)
}

func (r *Dir) List(path string) iter.Seq2[os.FileInfo, error] {
	return func(yield func(os.FileInfo, error) bool) {
		files, err := r.client.ReadDir(path)
		if err != nil {
			yield(nil, err)
			return
		}
		for _, f := range files {
			if !yield(f, err) {
				return
			}
		}
	}
}

func (r *Dir) Download(path string) (*File, error) {
	data, err := r.client.Read(path)
	if err != nil {
		return nil, err
	}

	return &File{
		Path: path,
		Data: data,
	}, nil

}

func (r *Dir) Delete(path string) error {
	return r.client.Remove(path)
}
