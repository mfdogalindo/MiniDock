package app

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"html/template"
	"net/http"
	"sync"
	"time"

	"github.com/julieta/minidock/internal/security"
	"github.com/julieta/minidock/internal/store"
)

//go:embed templates/*.html
var templateFiles embed.FS

type App struct {
	config    Config
	store     *store.Store
	templates *template.Template
	mu        sync.RWMutex
	key       []byte
	sessions  map[string]time.Time
}

func New(config Config, database *store.Store) (*App, error) {
	templates, err := template.ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{config: config, store: database, templates: templates, sessions: make(map[string]time.Time)}, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /readyz", a.ready)
	mux.HandleFunc("GET /", a.dashboard)
	mux.HandleFunc("GET /setup", a.setupForm)
	mux.HandleFunc("POST /setup", a.setup)
	mux.HandleFunc("GET /unlock", a.unlockForm)
	mux.HandleFunc("POST /unlock", a.unlock)
	mux.HandleFunc("POST /lock", a.lock)
	return a.securityHeaders(mux)
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (a *App) ready(w http.ResponseWriter, r *http.Request) {
	initialized, err := a.store.IsInitialized(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if !initialized {
		http.Error(w, "setup required", http.StatusServiceUnavailable)
		return
	}
	a.mu.RLock()
	unlocked := len(a.key) > 0
	a.mu.RUnlock()
	if !unlocked {
		http.Error(w, "unlock required", http.StatusServiceUnavailable)
		return
	}
	a.health(w, r)
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	initialized, err := a.store.IsInitialized(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if !initialized {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if !a.authorized(r) {
		http.Redirect(w, r, "/unlock", http.StatusSeeOther)
		return
	}
	a.render(w, "dashboard.html", map[string]string{"Environment": a.config.Environment})
}

func (a *App) setupForm(w http.ResponseWriter, r *http.Request) {
	initialized, err := a.store.IsInitialized(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if initialized {
		http.Redirect(w, r, "/unlock", http.StatusSeeOther)
		return
	}
	a.render(w, "setup.html", map[string]string{})
}

func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	password := r.Form.Get("password")
	if len(password) < 12 || password != r.Form.Get("password_confirmation") {
		a.renderError(w, "setup.html", "Usa una contraseña de al menos 12 caracteres y confirma el mismo valor.")
		return
	}
	salt, err := security.NewSalt()
	if err != nil {
		http.Error(w, "could not initialize security", http.StatusInternalServerError)
		return
	}
	key, err := security.DeriveKey(password, salt)
	if err != nil {
		http.Error(w, "could not derive key", http.StatusInternalServerError)
		return
	}
	defer security.Zero(key)
	nonce, cipher, err := security.NewVerifier(key)
	if err != nil {
		http.Error(w, "could not initialize verifier", http.StatusInternalServerError)
		return
	}
	if err := a.store.InitializeSecurity(r.Context(), store.SecurityConfig{Salt: salt, VerifierNonce: nonce, VerifierCipher: cipher}); err != nil {
		a.renderError(w, "setup.html", "La configuración ya existe o no pudo guardarse.")
		return
	}
	a.setKey(key)
	a.newSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) unlockForm(w http.ResponseWriter, r *http.Request) {
	initialized, err := a.store.IsInitialized(r.Context())
	if err != nil || !initialized {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if a.authorized(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, "unlock.html", map[string]string{})
}

func (a *App) unlock(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	config, err := a.store.SecurityConfig(r.Context())
	if err != nil {
		http.Error(w, "security configuration unavailable", http.StatusServiceUnavailable)
		return
	}
	key, err := security.DeriveKey(r.Form.Get("password"), config.Salt)
	if err != nil {
		http.Error(w, "could not derive key", http.StatusInternalServerError)
		return
	}
	if err := security.ValidateKey(key, config.VerifierNonce, config.VerifierCipher); err != nil {
		security.Zero(key)
		a.renderError(w, "unlock.html", "Contraseña incorrecta.")
		return
	}
	a.setKey(key)
	security.Zero(key)
	a.newSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) lock(w http.ResponseWriter, r *http.Request) {
	a.Lock()
	cookie, err := r.Cookie("minidock_session")
	if err == nil {
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "minidock_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/unlock", http.StatusSeeOther)
}

func (a *App) Lock() {
	a.mu.Lock()
	defer a.mu.Unlock()
	security.Zero(a.key)
	a.key = nil
	a.sessions = make(map[string]time.Time)
}

func (a *App) setKey(key []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	security.Zero(a.key)
	a.key = append(a.key[:0], key...)
}

func (a *App) authorized(r *http.Request) bool {
	cookie, err := r.Cookie("minidock_session")
	if err != nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.key) == 0 {
		return false
	}
	expires, ok := a.sessions[cookie.Value]
	return ok && time.Now().Before(expires)
}

func (a *App) newSession(w http.ResponseWriter) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	session := base64.RawURLEncoding.EncodeToString(value)
	expires := time.Now().Add(12 * time.Hour)
	a.mu.Lock()
	a.sessions[session] = expires
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "minidock_session", Value: session, Path: "/", Expires: expires, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (a *App) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "render template", http.StatusInternalServerError)
	}
}

func (a *App) renderError(w http.ResponseWriter, name, message string) {
	w.WriteHeader(http.StatusBadRequest)
	a.render(w, name, map[string]string{"Error": message})
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
