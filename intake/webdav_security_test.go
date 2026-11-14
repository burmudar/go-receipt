package intake

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/receipt"
)

func TestWebDAVEndpointConfinement(t *testing.T) {
	var hits atomic.Int32
	outsider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer outsider.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, outsider.URL+"/stolen", http.StatusFound)
	}))
	defer redirect.Close()
	input, err := NewWebDAVInput(redirect.URL+"/dav/", "user", "sensitive-password", "inbox")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.Files(context.Background()); err == nil {
		t.Fatal("redirect should not be followed")
	}
	for _, location := range []string{
		outsider.URL + "/dav/inbox/file.pdf",
		redirect.URL + "/outside/file.pdf",
		redirect.URL + "/dav/%2e%2e/outside.pdf",
		redirect.URL + "/dav/inbox/%2e%2e/%2e%2e/outside.pdf",
		"http://user:secret@" + strings.TrimPrefix(redirect.URL, "http://") + "/dav/inbox/x",
	} {
		if _, err := input.Read(context.Background(), location); err == nil {
			t.Fatalf("accepted invalid source %s", location)
		}
	}
	var authenticated atomic.Bool
	guarded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "user" || pass != "sensitive-password" {
			w.Header().Set("WWW-Authenticate", `Basic realm="dav"`)
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		authenticated.Store(true)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, outsider.URL+"/stolen", http.StatusFound)
	}))
	defer guarded.Close()
	protected, err := NewWebDAVInput(guarded.URL+"/dav/", "user", "sensitive-password", "inbox")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protected.Read(context.Background(), guarded.URL+"/dav/inbox/file.pdf"); err == nil {
		t.Fatal("redirected document should fail")
	}
	if !authenticated.Load() {
		t.Fatal("fixture did not test authenticated requests")
	}
	if hits.Load() != 0 {
		t.Fatalf("sent %d requests to another endpoint", hits.Load())
	}
	for _, inbox := range []string{"../outside", "/absolute", "a/../b", "a\\b"} {
		if _, err := NewWebDAVInput(redirect.URL+"/dav/", "user", "password", inbox); err == nil {
			t.Fatalf("accepted inbox %q", inbox)
		}
	}
	if _, err := NewWebDAVInput("http://user:password@example.org/dav", "user", "password", "inbox"); err == nil {
		t.Fatal("accepted URL credentials")
	}
}

func TestWebDAVCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}))
	defer srv.Close()
	defer close(release)
	input, err := NewWebDAVInput(srv.URL+"/dav/", "user", "password", "inbox")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := input.Files(ctx)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WebDAV request did not stop on cancellation")
	}
}

func TestSourceConfigurationErrors(t *testing.T) {
	source := MixedSource{}
	_, err := source.Read(context.Background(), "https://example.org/dav/inbox/file.pdf")
	if !errors.Is(err, receipt.ErrSourceUnconfigured) {
		t.Fatalf("not classified as unconfigured: %v", err)
	}
}
