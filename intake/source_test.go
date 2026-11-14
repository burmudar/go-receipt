package intake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/burmudar/go-receipt/receipt"
)

func TestWebDAVDiscoveryFileDisappearsAfterListing(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		stillListed  bool
		recheckFails bool
		wantError    bool
	}{
		{"removed after listing", http.StatusNotFound, false, false, false},
		{"404 but still listed", http.StatusNotFound, true, false, true},
		{"inbox recheck fails", http.StatusNotFound, false, true, true},
		{"permission failure", http.StatusForbidden, false, false, true},
		{"server failure", http.StatusInternalServerError, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listings := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodOptions:
					w.Header().Set("DAV", "1, 2")
				case "PROPFIND":
					listings++
					if listings > 1 && tc.recheckFails {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Type", "application/xml")
					w.WriteHeader(http.StatusMultiStatus)
					fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>/dav/inbox/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
					if listings == 1 || tc.stillListed {
						fmt.Fprint(w, `<d:response><d:href>/dav/inbox/gone.pdf</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>5</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
					}
					fmt.Fprint(w, `<d:response><d:href>/dav/inbox/retained.pdf</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>5</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
				case http.MethodGet:
					if r.URL.Path == "/dav/inbox/gone.pdf" {
						w.WriteHeader(tc.status)
					} else {
						fmt.Fprint(w, "valid")
					}
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer server.Close()
			input, err := NewWebDAVInput(server.URL+"/dav/", "user", "password", "inbox")
			if err != nil {
				t.Fatal(err)
			}
			files, err := input.Files(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			count, failures := 0, 0
			for file, err := range files {
				if err != nil {
					if !errors.Is(err, receipt.ErrSourceUnavailable) {
						t.Fatalf("wrong error: %v", err)
					}
					if tc.recheckFails && !strings.Contains(err.Error(), "recheck WebDAV inbox") {
						t.Fatalf("missing recheck error: %v", err)
					}
					if !tc.recheckFails && !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", tc.status)) {
						t.Fatalf("missing HTTP status: %v", err)
					}
					failures++
				} else if file.Name == "retained.pdf" {
					count++
				} else {
					t.Fatalf("unexpected receipt: %q", file.Name)
				}
			}
			if count != 1 || (failures != 0) != tc.wantError {
				t.Fatalf("retained=%d failures=%d, want one retained and error=%v", count, failures, tc.wantError)
			}
		})
	}
}

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

			endpoint := server.URL + "/dav/"
			reader, err := NewWebDAVInput(endpoint, "fixture-user", "fixture-password", "inbox")
			if err != nil {
				t.Fatal(err)
			}
			files, err := reader.Files(ctx)
			if err != nil {
				t.Fatal(err)
			}
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
