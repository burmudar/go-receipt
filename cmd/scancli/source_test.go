package scancli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestWebDAVSourceRoundTrip(t *testing.T) {
	for _, name := range []string{"discount 10%.pdf", "receipt%20copy.pdf"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			data := []byte("%PDF-1.4\n" + name + "\n%%EOF")
			filePath := "/dav/inbox/" + name
			fileURL := &url.URL{Path: filePath}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodOptions:
					w.Header().Set("DAV", "1, 2")
				case "PROPFIND":
					w.Header().Set("Content-Type", "application/xml")
					w.WriteHeader(http.StatusMultiStatus)
					fmt.Fprintf(w, `<d:multistatus xmlns:d="DAV:">
						<d:response>
							<d:href>/dav/inbox/</d:href>
							<d:propstat>
								<d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop>
								<d:status>HTTP/1.1 200 OK</d:status>
							</d:propstat>
						</d:response>
						<d:response>
							<d:href>%s</d:href>
							<d:propstat>
								<d:prop>
									<d:resourcetype/>
									<d:getcontentlength>%d</d:getcontentlength>
								</d:prop>
								<d:status>HTTP/1.1 200 OK</d:status>
							</d:propstat>
						</d:response>
					</d:multistatus>`, fileURL.EscapedPath(), len(data))
				case http.MethodGet:
					if r.URL.Path != filePath {
						http.NotFound(w, r)
						return
					}
					w.Write(data)
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer server.Close()

			t.Setenv("WEBDAV_PASSWORD", "fixture-password")
			endpoint := server.URL + "/dav/"
			files, err := webDAVFiles(ctx, endpoint, "fixture-user", "fixture-password", "inbox")
			if err != nil {
				t.Fatal(err)
			}

			reader := &sourceReader{config: config{
				endpoint: endpoint,
				user:     "fixture-user",
			}}
			count := 0
			for file, err := range files {
				if err != nil {
					t.Fatal(err)
				}
				count++
				if file.Name != name || !bytes.Equal(file.Data, data) {
					t.Fatalf("discovery returned the wrong receipt: %q", file.Name)
				}

				reloaded, err := reader.Read(ctx, file.Source)
				if err != nil {
					t.Fatalf("reload %q from %q: %v", name, strings.TrimPrefix(file.Source, server.URL), err)
				}
				if !bytes.Equal(reloaded, data) {
					t.Fatal("persisted source reference returned different receipt bytes")
				}
			}
			if count != 1 {
				t.Fatalf("discovered %d receipts, want 1", count)
			}
		})
	}
}
