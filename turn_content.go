package main

import (
	"encoding/base64"
	"fmt"
	"mime"
	"strings"
)

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
	Typ    string `json:"type"`
	Name   string `json:"filename"`
	Data   string `json:"file_data"`
	Detail string `json:"detail"`
}

type ImageContent struct {
	Typ  string `json:"type"`
	Data string `json:"image_url"`
}

func NewTextContent(text string) *TextContent {
	return &TextContent{
		Typ:  "input_text",
		Text: text,
	}
}

func NewMimeTypeContent(name string, data []byte, ext string) TurnContent {
	mimeType := mime.TypeByExtension(ext)

	encData := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data))

	if strings.HasPrefix(mimeType, "image") {
		return &ImageContent{
			Typ:  "input_image",
			Data: encData,
		}
	}

	return &FileContent{
		Typ:    "input_file",
		Name:   name,
		Data:   encData,
		Detail: "auto",
	}
}

func (c *TextContent) Type() string  { return c.Typ }
func (c *ImageContent) Type() string { return c.Typ }
func (c *FileContent) Type() string  { return c.Typ }
