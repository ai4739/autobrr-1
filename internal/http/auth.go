package http

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi"
	"github.com/gorilla/sessions"

	"github.com/autobrr/autobrr/internal/domain"
)

type authService interface {
	Login(ctx context.Context, username, password string) (*domain.User, error)
}

type authHandler struct {
	encoder encoder
	config  domain.Config
	service authService

	cookieStore *sessions.CookieStore
}

func newAuthHandler(encoder encoder, config domain.Config, cookieStore *sessions.CookieStore, service authService) *authHandler {
	return &authHandler{
		encoder:     encoder,
		config:      config,
		service:     service,
		cookieStore: cookieStore,
	}
}

func (h authHandler) Routes(r chi.Router) {
	r.Post("/login", h.login)
	r.Post("/logout", h.logout)
	r.Get("/test", h.test)
}

func (h authHandler) login(w http.ResponseWriter, r *http.Request) {
	var (
		ctx  = r.Context()
		data domain.User
	)

	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		// encode error
		h.encoder.StatusResponse(ctx, w, nil, http.StatusBadRequest)
		return
	}

	h.cookieStore.Options.HttpOnly = true
	h.cookieStore.Options.SameSite = http.SameSiteLaxMode
	h.cookieStore.Options.Path = h.config.BaseURL

	// autobrr does not support serving on TLS / https, so this is only available behind reverse proxy
	// if forwarded protocol is https then set cookie secure
	// SameSite Strict can only be set with a secure cookie. So we overwrite it here if possible.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Set-Cookie/SameSite
	fwdProto := r.Header.Get("X-Forwarded-Proto")
	if fwdProto == "https" {
		h.cookieStore.Options.Secure = true
		h.cookieStore.Options.SameSite = http.SameSiteStrictMode
	}

	session, _ := h.cookieStore.Get(r, "user_session")

	_, err := h.service.Login(ctx, data.Username, data.Password)
	if err != nil {
		h.encoder.StatusResponse(ctx, w, nil, http.StatusUnauthorized)
		return
	}

	// Set user as authenticated
	session.Values["authenticated"] = true
	session.Save(r, w)

	h.encoder.StatusResponse(ctx, w, nil, http.StatusNoContent)
}

func (h authHandler) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	session, _ := h.cookieStore.Get(r, "user_session")

	// Revoke users authentication
	session.Values["authenticated"] = false
	session.Save(r, w)

	h.encoder.StatusResponse(ctx, w, nil, http.StatusNoContent)
}

func (h authHandler) test(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	session, _ := h.cookieStore.Get(r, "user_session")

	// Check if user is authenticated
	if auth, ok := session.Values["authenticated"].(bool); !ok || !auth {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// send empty response as ok
	h.encoder.StatusResponse(ctx, w, nil, http.StatusNoContent)
}
