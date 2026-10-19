package remotefs

import (
	"crypto/sha256"
	"iter"
	"os"
	"path/filepath"
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
	client := gowebdav.NewClient(endpoint, user, pass)
	client.SetTimeout(30 * time.Second)

	if err := client.Connect(); err != nil {
		return nil, err
	}

	return &Dir{
		client: client,
	}, nil
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
