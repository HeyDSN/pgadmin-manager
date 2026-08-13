package dashboard

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

//go:embed web/templates/*.html web/static/*
var webFiles embed.FS

type Handler struct {
	service   *PostgresService
	logger    *log.Logger
	csrfToken string
	templates map[string]*template.Template
	static    http.Handler
}

type CountRow struct {
	Name  string
	Count int
}

type AccessChoice struct {
	Name    string
	Checked bool
	Status  string
}

type PageData struct {
	Title            string
	Active           string
	CSRFToken        string
	Success          string
	Error            string
	Users            []User
	Databases        []Database
	Preview          *MigrationPlan
	PreviewCounts    []CountRow
	ServerStatus     string
	TotalUsers       int
	TotalDatabases   int
	ManagedDatabases int
	LegacyDatabases  int
	ShowAdd          bool
	ManagedUser      string
	AccessChoices    []AccessChoice
}

func NewHandler(service *PostgresService, logger *log.Logger) (http.Handler, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("create CSRF token: %w", err)
	}

	h := &Handler{
		service: service, logger: logger,
		csrfToken: hex.EncodeToString(tokenBytes),
		templates: make(map[string]*template.Template),
	}
	for _, page := range []string{"dashboard.html", "users.html", "databases.html"} {
		tmpl, err := template.ParseFS(webFiles, "web/templates/layout.html", "web/templates/"+page)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		h.templates[page] = tmpl
	}
	staticFS, err := fs.Sub(webFiles, "web/static")
	if err != nil {
		return nil, err
	}
	h.static = http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.dashboard)
	mux.HandleFunc("GET /users", h.users)
	mux.HandleFunc("POST /users", h.createUser)
	mux.HandleFunc("POST /users/delete", h.deleteUser)
	mux.HandleFunc("POST /users/access", h.syncUserAccess)
	mux.HandleFunc("GET /databases", h.databases)
	mux.HandleFunc("POST /databases", h.createDatabase)
	mux.HandleFunc("POST /databases/full-access", h.grantFullAccess)
	mux.HandleFunc("POST /databases/revoke-access", h.revokeFullAccess)
	mux.HandleFunc("POST /databases/access", h.syncDatabaseAccess)
	mux.HandleFunc("GET /healthz", h.health)
	mux.Handle("GET /static/", h.static)
	return h.securityHeaders(h.requestLog(mux)), nil
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	users, userErr := h.service.ListUsers(r.Context())
	databases, databaseErr := h.service.ListDatabases(r.Context())
	data := PageData{Title: "Dashboard", Active: "dashboard", CSRFToken: h.csrfToken, ServerStatus: "Connected"}
	if userErr != nil || databaseErr != nil {
		data.ServerStatus = "Error"
		data.Error = joinErrors(userErr, databaseErr)
	}
	data.TotalUsers = len(users)
	data.TotalDatabases = len(databases)
	for _, database := range databases {
		if database.Status == "Managed" {
			data.ManagedDatabases++
		} else {
			data.LegacyDatabases++
		}
	}
	h.render(w, http.StatusOK, "dashboard.html", data)
}

func (h *Handler) users(w http.ResponseWriter, r *http.Request) {
	h.renderUsers(w, r, http.StatusOK, "")
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	if !h.validForm(w, r) {
		return
	}
	username := r.FormValue("username")
	err := h.service.CreateUser(r.Context(), username, r.FormValue("password"))
	if err != nil {
		r.URL.RawQuery = "add=1"
		h.renderUsers(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	redirectSuccess(w, r, "/users", fmt.Sprintf("User %s created", username))
}

func (h *Handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !h.validForm(w, r) {
		return
	}
	if r.FormValue("confirmed") != "yes" {
		http.Error(w, "confirmation is required", http.StatusBadRequest)
		return
	}
	username := r.FormValue("username")
	if err := h.service.DeleteUser(r.Context(), username); err != nil {
		h.renderUsers(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	redirectSuccess(w, r, "/users", fmt.Sprintf("User %s deleted", username))
}

func (h *Handler) syncUserAccess(w http.ResponseWriter, r *http.Request) {
	if !h.validForm(w, r) {
		return
	}
	if r.FormValue("confirmed") != "yes" {
		http.Error(w, "confirmation is required", http.StatusBadRequest)
		return
	}
	username := r.FormValue("username")
	users, err := h.service.ListUsers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var current []string
	found := false
	for _, user := range users {
		if user.Name == username {
			if user.Superuser {
				http.Error(w, "administrative users cannot be managed here", http.StatusBadRequest)
				return
			}
			found = true
			current = user.FullAccessDatabases
			break
		}
	}
	if !found {
		http.Error(w, "login user does not exist", http.StatusBadRequest)
		return
	}
	if err := syncMappings(current, r.Form["databases"],
		func(database string) error {
			_, err := h.service.GrantFullAccess(r.Context(), database, username)
			return err
		},
		func(database string) error { return h.service.RevokeFullAccess(r.Context(), database, username) },
	); err != nil {
		r.URL.RawQuery = url.Values{"manage": {username}}.Encode()
		h.renderUsers(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	redirectSuccess(w, r, "/users", fmt.Sprintf("Database access updated for %s", username))
}

func (h *Handler) renderUsers(w http.ResponseWriter, r *http.Request, status int, actionError string) {
	users, userErr := h.service.ListUsers(r.Context())
	databases, databaseErr := h.service.ListDatabases(r.Context())
	data := PageData{
		Title: "Users", Active: "users", CSRFToken: h.csrfToken,
		Users: users, Databases: databases, Success: r.URL.Query().Get("success"),
		Error:   joinErrors(errorFromString(actionError), userErr, databaseErr),
		ShowAdd: r.URL.Query().Get("add") == "1",
	}
	if managedUser := r.URL.Query().Get("manage"); managedUser != "" {
		data.ManagedUser = managedUser
		mapped := map[string]bool{}
		found := false
		for _, user := range users {
			if user.Name == managedUser && !user.Superuser {
				found = true
				for _, database := range user.FullAccessDatabases {
					mapped[database] = true
				}
				break
			}
		}
		if !found {
			data.Error = joinErrors(errorFromString(data.Error), fmt.Errorf("login user %q does not exist", managedUser))
			data.ManagedUser = ""
		} else {
			for _, database := range databases {
				data.AccessChoices = append(data.AccessChoices, AccessChoice{Name: database.Name, Checked: mapped[database.Name], Status: database.Status})
			}
		}
	}
	h.render(w, status, "users.html", data)
}

func (h *Handler) databases(w http.ResponseWriter, r *http.Request) {
	h.renderDatabases(w, r, http.StatusOK, "")
}

func (h *Handler) createDatabase(w http.ResponseWriter, r *http.Request) {
	if !h.validForm(w, r) {
		return
	}
	database := r.FormValue("database")
	if err := h.service.CreateDatabase(r.Context(), database); err != nil {
		r.URL.RawQuery = "add=1"
		h.renderDatabases(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	redirectSuccess(w, r, "/databases", fmt.Sprintf("Database %s created", database))
}

func (h *Handler) syncDatabaseAccess(w http.ResponseWriter, r *http.Request) {
	if !h.validForm(w, r) {
		return
	}
	if r.FormValue("confirmed") != "yes" {
		http.Error(w, "confirmation is required", http.StatusBadRequest)
		return
	}
	database := r.FormValue("database")
	databases, err := h.service.ListDatabases(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var current []string
	found := false
	for _, item := range databases {
		if item.Name == database {
			found = true
			current = item.FullAccessUsers
			break
		}
	}
	if !found {
		http.Error(w, "database does not exist", http.StatusBadRequest)
		return
	}
	users, err := h.service.ListUsers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	eligible := map[string]bool{}
	protected := map[string]bool{}
	for _, user := range users {
		if user.Superuser {
			protected[user.Name] = true
		} else {
			eligible[user.Name] = true
		}
	}
	selected := append([]string(nil), r.Form["users"]...)
	for _, username := range selected {
		if !eligible[username] {
			http.Error(w, "selected user is not eligible for managed access", http.StatusBadRequest)
			return
		}
	}
	for _, username := range current {
		if protected[username] {
			selected = append(selected, username)
		}
	}
	if err := syncMappings(current, selected,
		func(username string) error {
			_, err := h.service.GrantFullAccess(r.Context(), database, username)
			return err
		},
		func(username string) error { return h.service.RevokeFullAccess(r.Context(), database, username) },
	); err != nil {
		r.URL.RawQuery = url.Values{"inspect": {database}}.Encode()
		h.renderDatabases(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	redirectSuccess(w, r, "/databases", fmt.Sprintf("Full-access users updated for %s", database))
}

func syncMappings(current, selected []string, grant, revoke func(string) error) error {
	currentSet := stringSet(current)
	selectedSet := stringSet(selected)
	changes := 0
	for _, value := range selected {
		if !currentSet[value] {
			if err := grant(value); err != nil {
				return mappingSyncError(changes, "grant", value, err)
			}
			currentSet[value] = true
			changes++
		}
	}
	for _, value := range current {
		if !selectedSet[value] {
			if err := revoke(value); err != nil {
				return mappingSyncError(changes, "revoke", value, err)
			}
			changes++
		}
	}
	return nil
}

func mappingSyncError(completed int, action, value string, err error) error {
	if completed > 0 {
		return fmt.Errorf("%d earlier change(s) completed before %s %q failed: %w", completed, action, value, err)
	}
	return fmt.Errorf("%s %q: %w", action, value, err)
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func (h *Handler) grantFullAccess(w http.ResponseWriter, r *http.Request) {
	if !h.validForm(w, r) {
		return
	}
	if r.FormValue("confirmed") != "yes" {
		http.Error(w, "confirmation is required", http.StatusBadRequest)
		return
	}
	database := r.FormValue("database")
	username := r.FormValue("username")
	if _, err := h.service.GrantFullAccess(r.Context(), database, username); err != nil {
		r.URL.RawQuery = url.Values{"inspect": {database}}.Encode()
		h.renderDatabases(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	redirectSuccess(w, r, "/databases", fmt.Sprintf("Full access to %s granted to %s", database, username))
}

func (h *Handler) revokeFullAccess(w http.ResponseWriter, r *http.Request) {
	if !h.validForm(w, r) {
		return
	}
	if r.FormValue("confirmed") != "yes" {
		http.Error(w, "confirmation is required", http.StatusBadRequest)
		return
	}
	database := r.FormValue("database")
	username := r.FormValue("username")
	if err := h.service.RevokeFullAccess(r.Context(), database, username); err != nil {
		r.URL.RawQuery = url.Values{"inspect": {database}}.Encode()
		h.renderDatabases(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	redirectSuccess(w, r, "/databases", fmt.Sprintf("Full access to %s revoked from %s", database, username))
}

func (h *Handler) renderDatabases(w http.ResponseWriter, r *http.Request, status int, actionError string) {
	databases, databaseErr := h.service.ListDatabases(r.Context())
	users, userErr := h.service.ListUsers(r.Context())
	data := PageData{
		Title: "Databases", Active: "databases", CSRFToken: h.csrfToken,
		Databases: databases, Users: users, Success: r.URL.Query().Get("success"),
		Error:   joinErrors(errorFromString(actionError), databaseErr, userErr),
		ShowAdd: r.URL.Query().Get("add") == "1",
	}
	if inspect := r.URL.Query().Get("inspect"); inspect != "" {
		preview, err := h.service.PreviewMigration(r.Context(), inspect)
		if err != nil {
			data.Error = joinErrors(errorFromString(data.Error), err)
		} else {
			data.Preview = preview
			mapped := map[string]bool{}
			for _, database := range databases {
				if database.Name == inspect {
					for _, username := range database.FullAccessUsers {
						mapped[username] = true
					}
					break
				}
			}
			for _, user := range users {
				if !user.Superuser {
					data.AccessChoices = append(data.AccessChoices, AccessChoice{Name: user.Name, Checked: mapped[user.Name]})
				}
			}
			for name, count := range preview.Counts {
				data.PreviewCounts = append(data.PreviewCounts, CountRow{Name: name, Count: count})
			}
			sort.Slice(data.PreviewCounts, func(i, j int) bool { return data.PreviewCounts[i].Name < data.PreviewCounts[j].Name })
		}
	}
	h.render(w, status, "databases.html", data)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if err := h.service.Ping(r.Context()); err != nil {
		http.Error(w, "postgresql unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (h *Handler) validForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return false
	}
	provided := r.FormValue("csrf_token")
	if subtle.ConstantTimeCompare([]byte(provided), []byte(h.csrfToken)) != 1 {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return false
	}
	return true
}

func (h *Handler) render(w http.ResponseWriter, status int, name string, data PageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.templates[name].ExecuteTemplate(w, "layout", data); err != nil {
		h.logger.Printf("render %s: %v", name, err)
	}
}

func (h *Handler) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		h.logger.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}

func redirectSuccess(w http.ResponseWriter, r *http.Request, path, message string) {
	http.Redirect(w, r, path+"?"+url.Values{"success": {message}}.Encode(), http.StatusSeeOther)
}

func joinErrors(errs ...error) string {
	var messages []string
	for _, err := range errs {
		if err != nil {
			messages = append(messages, err.Error())
		}
	}
	return strings.Join(messages, "; ")
}

func errorFromString(message string) error {
	if message == "" {
		return nil
	}
	return fmt.Errorf("%s", message)
}
