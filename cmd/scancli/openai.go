package scancli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/burmudar/go-receipt/remotefs"

	"github.com/invopop/jsonschema"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

const ReceiptScannerPrompt = `You are analyzing receipts. Each receipt has items on it, a merchant, optionally a tip and a total. Analyze the receipt and report on:

The name of the merchant: As far as possible just the merchant. For instance Bootlegger - Ndabeni is just Bootlegger. Checkers Sixty60, is just Checkers
The receipt total amount without currency
Internal Currency unit eg. ZAR, USD
The date of the transaction: ISO 8601 Timestamp
Confidence on how accurate the analysis is as a value between 0.00 and 1.00


Respond with the following json:
	{ "merchant": <value>, "total: <value>, "currency": <value>, "date": <date>, "confidence": <confidence>, }
`

type ReceiptAnalysis struct {
	Merchant   string    `json:"merchant" jsonschema_description:"The marchant the transaction was performed at"`
	Total      float64   `json:"total" jsonschema_description:"The total receipt amount"`
	Currency   string    `json:"currency" jsonschema_description:"The currency unit of the receipt"`
	Date       time.Time `json:"date" jsonschema_description:"The date of the transaction"`
	Confidence float64   `json:"float64" jsonschema_description:"confidence score in the scan analysis"`
}

type ReceiptResult struct {
	File     *remotefs.File
	SHA      [32]byte
	Analysis ReceiptAnalysis
}

func (r *ReceiptResult) String() string {
	return fmt.Sprintf("<filename: %s, sha: %s, analysis: %+v>", r.File.Path, hex.EncodeToString(r.SHA[:]), r.Analysis)
}

type ReceiptScanner struct {
	prompt       string
	resultSchema map[string]any
	client       openai.Client
}

func NewReceiptScanner(token string) (*ReceiptScanner, error) {
	schema, err := GenerateSchema[ReceiptAnalysis]()
	if err != nil {
		return nil, err
	}
	return &ReceiptScanner{
		client:       openai.NewClient(option.WithAPIKey(token)),
		resultSchema: schema,
		prompt:       ReceiptScannerPrompt,
	}, nil
}

func asDataURL(mimeType string, data []byte) string {
	return fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data))
}

// Structured Outputs uses a subset of JSON schema
// These flags are necessary to comply with the subset
func GenerateSchema[T any]() (map[string]any, error) {
	reflector := jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
	}
	var v T
	schema := reflector.Reflect(v)

	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	// Preserve schema values as raw JSON so integer constraints are not rounded
	// through float64 before the SDK serializes the request.
	var rawSchema map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSchema); err != nil {
		return nil, err
	}
	result := make(map[string]any, len(rawSchema))
	for key, value := range rawSchema {
		result[key] = value
	}
	return result, nil
}

func (r *ReceiptScanner) Scan(ctx context.Context, file *remotefs.File) (*ReceiptResult, error) {
	contentParam := responses.ResponseInputContentUnionParam{}
	mimeType := mime.TypeByExtension(file.Ext())
	fmt.Printf("#### %v", mimeType)
	if strings.HasPrefix("image", mimeType) {
		contentParam.OfInputImage = &responses.ResponseInputImageParam{
			Detail:   "auto",
			FileID:   openai.String(file.Name()),
			ImageURL: openai.String(asDataURL(mimeType, file.Data)),
		}
	} else if strings.HasPrefix("application/pdf", mimeType) {
		contentParam.OfInputFile = &responses.ResponseInputFileParam{
			Filename: openai.String(file.Name()),
			FileData: openai.String(asDataURL(mimeType, file.Data)),
		}
	}

	resp, err := r.client.Responses.New(ctx, responses.ResponseNewParams{
		Model:        openai.ChatModelGPT5_5,
		Instructions: openai.String(r.prompt),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: responses.ResponseInputParam{
				responses.ResponseInputItemParamOfMessage(
					responses.ResponseInputMessageContentListParam{
						contentParam,
					},
					responses.EasyInputMessageRoleUser,
				),
			},
		},
		Text: responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigUnionParam{
				OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
					Name:        "receipt_scan_result",
					Description: openai.String("result of anaylyzing the receipt for transaction information"),
					Schema:      r.resultSchema,
					Strict:      openai.Bool(true),
				},
			},
		},
	})

	if err != nil {
		return nil, err
	}

	result := ReceiptResult{
		File:     file,
		SHA:      file.SHA(),
		Analysis: ReceiptAnalysis{},
	}

	if err := resp.Error; err.Message != "" {
		return nil, fmt.Errorf(err.Message)
	}
	if err := json.Unmarshal([]byte(resp.OutputText()), &result.Analysis); err != nil {
		return nil, err
	}

	return &result, err
}

func scan() error {
	endpoint := "https://storage.internal.fortkickass.dev"
	user := "william"
	password, err := os.ReadFile(".storage-password")
	if err != nil {
		panic(err)
	}
	password = bytes.TrimSpace(password)
	println(user, string(password), endpoint)
	dir, err := remotefs.NewDir(user, string(password), endpoint)
	if err != nil {
		panic(err)
	}

	token, err := os.ReadFile(".openai-password")
	if err != nil {
		panic(err)
	}
	token = bytes.TrimSpace(token)

	scanner, err := NewReceiptScanner(string(token))
	if err != nil {
		panic(err)
	}

	fmt.Println("======= INBOX ========")
	count := 0
	ctx := context.Background()
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
		fmt.Printf("SCANNING: %s\n", rf.Name())
		result, err := scanner.Scan(ctx, rf)
		if err != nil {
			fmt.Printf("scanning error: %v", err.Error())
		}

		fmt.Printf("%s", result)

		if count == 3 {
			break
		}
	}
	return nil
}
