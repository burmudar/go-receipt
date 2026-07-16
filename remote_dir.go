package main

import (
	"iter"
	"os"

	"github.com/studio-b12/gowebdav"
)

type RemoteDir struct {
	client *gowebdav.Client
}

type RemoteFile struct {
	Path string
	Data []byte
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
