package main

import "encoding/base64"

type Turn struct {
	Role    string        `json:"role"`
	Content []TurnContent `json:"content"`
}

type TurnContent interface {
	Type() string
}

type TextContent struct {
	Typ  string `json:"type"`
	Text string `json:"text"`
}

type FileContent struct {
	Typ  string `json:"type"`
	Name string `json:"filename"`
	Data string `json:"file_data"`
}

type ImageContent struct {
	Typ  string `json:"type"`
	Data string `json:"image_data"`
}

func NewTextContent(text string) *TextContent {
	return &TextContent{
		Typ:  "text",
		Text: text,
	}
}

func NewFileContent(name string, data []byte) *FileContent {
	return &FileContent{
		Typ:  "input_file",
		Name: name,
		Data: base64.RawStdEncoding.EncodeToString(data),
	}
}
