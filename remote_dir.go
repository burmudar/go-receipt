package main

import (
	"crypto/sha256"
	"iter"
	"os"
	"path/filepath"

	"github.com/studio-b12/gowebdav"
)

type RemoteDir struct {
	client *gowebdav.Client
}

type RemoteFile struct {
	Path string
	Data []byte
}

func (r *RemoteFile) Name() string {
	name := filepath.Base(r.Path)
	return name
}

func (r *RemoteFile) Content() []byte {
	return r.Data
}

func (r *RemoteFile) SHA() [32]byte {
	return sha256.Sum256(r.Data)
}

func (r *RemoteFile) Ext() string {
	return filepath.Ext(r.Path)
}

func NewRemoteDir(user, pass, endpoint string) (*RemoteDir, error) {
	client := gowebdav.NewClient(endpoint, user, pass)

	if err := client.Connect(); err != nil {
		return nil, err
	}

	return &RemoteDir{
		client: client,
	}, nil
}

func (r *RemoteDir) List(path string) iter.Seq2[os.FileInfo, error] {
	return func(yield func(os.FileInfo, error) bool) {
		files, err := r.client.ReadDir(path)
		for _, f := range files {
			if !yield(f, err) {
				return
			}
		}
	}
}

func (r *RemoteDir) Download(path string) (*RemoteFile, error) {
	data, err := r.client.Read(path)
	if err != nil {
		return nil, err
	}

	return &RemoteFile{
		Path: path,
		Data: data,
	}, nil

}

func (r *RemoteDir) Delete(path string) error {
	return r.client.Remove(path)
}
