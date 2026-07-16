package main

import (
	"fmt"
	"net/http"
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

func (l *LLMOCRScanner) Scan(content *RemoteFile) *OCRResult {
	llmReq := LLMRequest{
		Model: "gpt-5.5",
		Input: []Turn{
	{
				Role: "user",
				Content: nil
			},
		}
	}
	req, err := http.NewRequest(http.MethodPost, l.endpoint,
}

func (l *LLMOCRScanner) doRequest(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", l.token))
	req.Header.Set("Content-Type", "application/json")
	return l.client.Do(req)
}

func main() {
	endpoint := "https://storage.internal.fortkickass.dev"
	user := "william"
	password := "b3zu!d3nh0ut copyparty"
	dir, err := NewRemoteDir(user, password, endpoint)
	if err != nil {
		panic(err)
	}
	fmt.Println("======= INBOX ========")
	for f, err := range dir.List("inbox") {
		if err != nil {
			break
		}
		fmt.Printf(" %s (%v)\n", f.Name(), f.ModTime())
	}
}
