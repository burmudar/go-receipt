package xero

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Login runs a short-lived loopback callback server. The redirect URI must be
// registered verbatim with Xero. State is random, expires with this invocation,
// and is consumed once. No authorization codes or tokens are returned to the browser.
func (c *Client) Login(ctx context.Context, output io.Writer) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, err := url.Parse(c.credentials.RedirectURI)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Port() == "" ||
		(u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
		return errors.New("Xero redirect URI must be http://localhost:PORT/PATH or http://127.0.0.1:PORT/PATH")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", u.Port()))
	if err != nil {
		return errors.New("cannot listen for Xero callback; check the redirect port is available")
	}
	defer listener.Close()
	return c.login(ctx, listener, u, output)
}

func (c *Client) login(ctx context.Context, listener net.Listener, redirect *url.URL, output io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	state := base64.RawURLEncoding.EncodeToString(randomBytes())
	authorize, err := url.Parse(c.authorizeURL)
	if err != nil {
		return errors.New("invalid Xero authorization endpoint")
	}
	query := authorize.Query()
	query.Set("response_type", "code")
	query.Set("client_id", c.credentials.ClientID)
	query.Set("redirect_uri", c.credentials.RedirectURI)
	query.Set("scope", scopes)
	query.Set("state", state)
	authorize.RawQuery = query.Encode()

	result := make(chan error, 1)
	var once sync.Once
	path := redirect.Path
	if path == "" {
		path = "/"
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 45 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			if r.Method != http.MethodGet || r.URL.Path != path || r.Host != redirect.Host {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			if len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
				http.Error(w, "Invalid OAuth state. Return to the original login link.", http.StatusBadRequest)
				return
			}
			used := true
			once.Do(func() {
				used = false
				var err error
				if q.Get("error") != "" {
					err = errors.New("Xero authorization was denied; run xero login again")
				} else if len(q["code"]) != 1 || q.Get("code") == "" {
					err = errors.New("Xero callback has no authorization code; run xero login again")
				} else {
					_, err = c.exchange(ctx, url.Values{"grant_type": {"authorization_code"},
						"code": {q.Get("code")}, "redirect_uri": {c.credentials.RedirectURI}}, grant{})
				}
				if err != nil {
					http.Error(w, "Xero login failed. Check the CLI for the next step.", http.StatusBadRequest)
				} else {
					fmt.Fprintln(w, "Xero login saved. Return to the CLI and select an organisation.")
				}
				result <- err
			})
			if used {
				http.Error(w, "This login was already used.", http.StatusBadRequest)
			}
		})}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	}()
	if _, err := fmt.Fprintf(output, "Open this URL in a browser on this computer:\n%s\n", authorize.String()); err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return errors.New("Xero login cancelled or timed out; run xero login again")
	case <-serveErr:
		return errors.New("Xero callback server stopped; run xero login again")
	}
}

func randomBytes() []byte {
	b := make([]byte, 32)
	// crypto/rand.Read always fills the buffer or terminates the process.
	_, _ = rand.Read(b)
	return b
}
