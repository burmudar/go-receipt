package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

type LLMOCRScanner struct {
	client   *http.Client
	token    string
	endpoint string
}

type LLMRequest struct {
	Model string `json:"model"`
	Input []Turn `json:"input"`
}

type OCRResult struct{}

func (l *LLMOCRScanner) Scan(file *RemoteFile) (*OCRResult, error) {
	llmReq := LLMRequest{
		Model: "gpt-5.5",
		Input: []Turn{
			{
				Role: "user",
				Content: []TurnContent{
					NewMimeTypeContent(file.Name(), file.Content(), file.Ext()),
					NewTextContent(`You are analyzing receipts. Each receipt has items on it, a merchant, optionally a tip and a total. Analyze the receipt and report on:

The name of the merchant: As far as possible just the merchant. For instance Bootlegger - Ndabeni is just Bootlegger. Checkers Sixty60, is just Checkers
The receipt total amount without currency
Internal Currency unit eg. ZAR, USD
The date of the transaction: ISO 8601 Timestamp

Respond with the following json:
{ "merchant": <value>, "total: <value>, "currency": <value>, "date": <date> }
`),
				}},
		},
	}

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)

	if err := enc.Encode(&llmReq); err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, l.endpoint, &buf)
	if err != nil {
		return nil, err
	}

	resp, err := l.doRequest(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Bad response: %d\nBody: N/A", resp.StatusCode)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 400 {
		return nil, fmt.Errorf("Bad response: %d\nBody: %s", resp.StatusCode, string(body))
	}

	fmt.Println("======= RESPONSE =========")
	fmt.Println(string(body))
	fmt.Println("======= ******* =========")

	return nil, nil
}

func (l *LLMOCRScanner) doRequest(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", l.token))
	req.Header.Set("Content-Type", "application/json")
	return l.client.Do(req)
}

func main() {
	endpoint := "https://storage.internal.fortkickass.dev"
	user := "william"
	password, err := os.ReadFile(".storage-password")
	if err != nil {
		panic(err)
	}
	password = bytes.TrimSpace(password)
	println(user, string(password), endpoint)
	dir, err := NewRemoteDir(user, string(password), endpoint)
	if err != nil {
		panic(err)
	}

	token, err := os.ReadFile(".openai-password")
	if err != nil {
		panic(err)
	}
	token = bytes.TrimSpace(token)
	scanner := LLMOCRScanner{
		client:   http.DefaultClient,
		token:    string(token),
		endpoint: "https://api.openai.com/v1/responses",
	}

	fmt.Println("======= INBOX ========")
	count := 0
	for f, err := range dir.List("inbox") {
		if err != nil {
			break
		}
		fmt.Printf(" %s (%v)\n", f.Name(), f.ModTime())

		rf, err := dir.Download(filepath.Join("inbox", f.Name()))
		if err != nil {
			panic(err)
		}
		count += 1
		scanner.Scan(rf)

		if count == 3 {
			break
		}
	}
}
