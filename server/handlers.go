package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/burmudar/go-receipt/xero"
)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login.html", errorPage{})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.auth.checkPassword(r.PostFormValue("password")) {
		// Slow down password guessing.
		time.Sleep(time.Second)
		s.render(w, http.StatusUnauthorized, "login.html", errorPage{Message: "The password is not correct."})
		return
	}
	s.auth.setSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		s.auth.clearSession(w)
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type statusPage struct {
	SetupError  string
	RedirectURL string
	Connection  xero.Connection
	Auth        bool
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	page := statusPage{RedirectURL: RedirectURL(s.cfg.PublicURL), Auth: s.auth != nil}
	if s.xero == nil {
		page.SetupError = s.setupErr.Error()
		s.render(w, http.StatusOK, "status.html", page)
		return
	}
	connection, err := s.xero.Connection(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	page.Connection = connection
	s.render(w, http.StatusOK, "status.html", page)
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	authURL, err := s.xero.Start(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if reason := query.Get("error"); reason != "" {
		s.render(w, http.StatusBadRequest, "error.html", errorPage{Message: "Xero did not authorise the connection: " + reason})
		return
	}

	err := s.xero.Complete(r.Context(), query.Get("state"), query.Get("code"))
	switch {
	case errors.Is(err, xero.ErrInvalidState):
		s.fail(w, http.StatusBadRequest, err)
	case err != nil:
		s.fail(w, http.StatusBadGateway, err)
	default:
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	// The status page shows the result, including a reconnect requirement.
	if err := s.xero.Check(r.Context()); err != nil {
		s.cfg.Logger.Warn("xero connection check failed", "error", err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.xero.Disconnect(r.Context()); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
