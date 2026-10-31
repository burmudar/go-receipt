// Package server is the long-running receipt service. It serves the browser UI.
package server

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/burmudar/go-receipt/db/oauth"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/burmudar/go-receipt/xero"
)

const callbackPath = "/xero/callback"

type Config struct {
	DB *sql.DB
	// Listen is the TCP address for the HTTP server, for example 127.0.0.1:11211.
	Listen string
	// PublicURL is the address that browsers use, for example the Caddy HTTPS address.
	PublicURL *url.URL
	// Password enables single-user authentication. It is required unless the service is local only.
	Password string
	// RequestTimeout limits reading and writing each incoming request.
	RequestTimeout time.Duration
	// XeroTimeout limits each request to Xero.
	XeroTimeout time.Duration
	// XeroEndpoints replaces the Xero URLs in tests.
	XeroEndpoints xero.Endpoints
	// CheckInterval is the time between background connection checks. Each check refreshes the token.
	CheckInterval time.Duration
	Logger        *slog.Logger
	// Receipt input is optional. When configured, both Source and Scanner are required.
	Discover            func(context.Context) (iter.Seq2[receipt.File, error], error)
	Source              receipt.Source
	Scanner             receipt.Scanner
	ReceiptInterval     time.Duration
	TransactionInterval time.Duration
}

type Server struct {
	cfg      Config
	auth     *auth
	xero     *xero.Manager
	setupErr error
	pages    *template.Template
	handler  http.Handler
}

//go:embed templates/*.html
var templates embed.FS

// AuthRequired reports whether the service can be reached from other devices.
// Requests through a local reverse proxy arrive from localhost, so the public URL is also checked.
func AuthRequired(listen string, publicURL *url.URL) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return true
	}
	return !isLoopback(host) || !isLoopback(publicURL.Hostname())
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// RedirectURL is the OAuth redirect URI to register for the Xero app.
func RedirectURL(publicURL *url.URL) string {
	return strings.TrimSuffix(publicURL.String(), "/") + callbackPath
}

func New(cfg Config) (*Server, error) {
	if cfg.PublicURL == nil || (cfg.PublicURL.Scheme != "http" && cfg.PublicURL.Scheme != "https") || cfg.PublicURL.Host == "" {
		return nil, fmt.Errorf("the public URL must be an absolute http or https URL")
	}
	if cfg.PublicURL.Scheme == "http" && !isLoopback(cfg.PublicURL.Hostname()) {
		return nil, fmt.Errorf("the public URL must use https unless it is a localhost URL; Xero rejects other http redirect URIs")
	}
	if AuthRequired(cfg.Listen, cfg.PublicURL) && cfg.Password == "" {
		return nil, fmt.Errorf("a password is required when the service is reachable from other devices")
	}
	if cfg.ReceiptInterval == 0 {
		cfg.ReceiptInterval = time.Minute
	}
	if cfg.TransactionInterval == 0 {
		cfg.TransactionInterval = 15 * time.Minute
	}
	if cfg.ReceiptInterval < 0 || cfg.TransactionInterval < 0 {
		return nil, fmt.Errorf("worker intervals must be positive")
	}
	if cfg.Discover != nil && (cfg.Source == nil || cfg.Scanner == nil) {
		return nil, fmt.Errorf("receipt source and scanner are required for background import")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.RequestTimeout <= 0 || cfg.XeroTimeout <= 0 || cfg.CheckInterval <= 0 {
		return nil, fmt.Errorf("timeouts and the check interval must be positive")
	}

	pages, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		return nil, err
	}
	a, err := newAuth(cfg.Password, cfg.PublicURL.Scheme == "https")
	if err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, auth: a, pages: pages}

	client, err := oauth.NewStore(cfg.DB).GetByName("Xero")
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.setupErr = fmt.Errorf("xero client credentials are not set; run \"rcpt oauth set\" and restart the service")
	case err != nil:
		return nil, fmt.Errorf("read xero client credentials (apply database migrations first): %w", err)
	default:
		s.xero, err = xero.NewManager(cfg.DB, xero.Config{
			ClientID:     client.ClientID,
			ClientSecret: client.ClientSecret,
			RedirectURL:  RedirectURL(cfg.PublicURL),
			Endpoints:    cfg.XeroEndpoints,
			HTTPClient:   &http.Client{Timeout: cfg.XeroTimeout},
			Logger:       cfg.Logger,
		})
		if err != nil {
			return nil, err
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.Handle("POST /logout", s.protect(s.logout))
	mux.Handle("GET /{$}", s.protect(s.status))
	mux.Handle("GET /receipts", s.protect(s.receipts))
	mux.Handle("GET /receipts/{hash}", s.protect(s.receiptDetail))
	mux.Handle("GET /receipts/{hash}/document", s.protect(s.receiptDocument))
	mux.Handle("POST /xero/connect", s.protectXero(s.connect))
	mux.Handle("GET "+callbackPath, s.protectXero(s.callback))
	mux.Handle("POST /xero/check", s.protectXero(s.check))
	mux.Handle("POST /xero/transactions/refresh", s.protectXero(s.refreshTransactions))
	mux.Handle("GET /xero/transactions", s.protectXero(s.transactions))
	mux.Handle("POST /xero/disconnect", s.protectXero(s.disconnect))

	csrf := http.NewCrossOriginProtection()
	if err := csrf.AddTrustedOrigin(cfg.PublicURL.Scheme + "://" + cfg.PublicURL.Host); err != nil {
		return nil, err
	}
	s.handler = csrf.Handler(mux)
	return s, nil
}

// ServeHTTP logs each request. It logs only the path, because the callback query contains the authorisation code.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.handler.ServeHTTP(rec, r)
	s.cfg.Logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the connection, for example to extend a write deadline.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Run serves HTTP until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       s.cfg.RequestTimeout,
		WriteTimeout:      s.cfg.RequestTimeout,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.cfg.Logger.Handler(), slog.LevelWarn),
	}

	workerCtx, stopWorker := context.WithCancel(ctx)
	var workers sync.WaitGroup
	start := func(fn func(context.Context)) { workers.Add(1); go func() { defer workers.Done(); fn(workerCtx) }() }
	defer func() { stopWorker(); workers.Wait() }()
	if s.xero != nil {
		start(s.checkPeriodically)
		start(s.refreshPeriodically)
	}
	if s.cfg.Discover != nil {
		start(s.processPeriodically)
	}

	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(listener) }()
	s.cfg.Logger.Info("serving", "listen", listener.Addr().String(), "public_url", s.cfg.PublicURL.String(), "auth", s.auth != nil)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	s.cfg.Logger.Info("shutting down")
	stopWorker()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// checkPeriodically keeps the refresh token in use. An unused Xero refresh token expires after 60 days.
func (s *Server) checkPeriodically(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		connection, err := s.xero.Connection(ctx)
		if err != nil || connection.Status != xero.StatusConnected {
			continue
		}
		if err := s.xero.Check(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("xero connection check failed", "error", err)
		}
	}
}

func (s *Server) protect(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth != nil && !s.auth.valid(r) {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	})
}

func (s *Server) protectXero(next http.HandlerFunc) http.Handler {
	return s.protect(func(w http.ResponseWriter, r *http.Request) {
		if s.xero == nil {
			s.render(w, http.StatusServiceUnavailable, "error.html", errorPage{Message: s.setupErr.Error()})
			return
		}
		next(w, r)
	})
}

type errorPage struct {
	Message string
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := s.pages.ExecuteTemplate(w, name, data); err != nil {
		s.cfg.Logger.Error("render page", "page", name, "error", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, status int, err error) {
	s.cfg.Logger.Warn("request failed", "error", err)
	s.render(w, status, "error.html", errorPage{Message: err.Error()})
}
