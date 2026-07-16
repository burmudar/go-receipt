package main

import (
	"fmt"
	"net/http"
	"time"
)

type DocumentStore struct {
	client   *http.Client
	token    string
	endpoint string
}

type Document struct {
	Title         string
	Created       time.Time
	Correspondent string
	Type          string
	Tags          []string
}

func NewDocumentStore(token, endpoint string) *DocumentStore {
	return &DocumentStore{
		client:   http.DefaultClient,
		token:    token,
		endpoint: endpoint,
	}
}

func (s *DocumentStore) doRequest(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", fmt.Sprintf("Token %s", s.token))
	return s.client.Do(req)
}

// https://docs.paperless-ngx.com/api/#file-uploads
func (s *DocumentStore) Add(d Document) error {
	req, err := http.NewRequest(http.MethodPost, s.endpoint, nil)
	if err != nil {
		return err
	}

	form := req.Form
	form.Set("title", d.Title)
	form.Set("created", d.Created.String())
	if d.Correspondent != "" {
		form.Set("correspondent", d.Correspondent)
	}
	if d.Type != "" {
		form.Set("document_type", d.Type)
	}

	resp, err := s.doRequest(req)
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 && resp.StatusCode >= 400 {
		return fmt.Errorf("failed to add document - status code %d\nADD BODY HERE", resp.StatusCode)
	}

	return nil
}
