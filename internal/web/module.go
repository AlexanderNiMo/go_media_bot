package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/fx"

	"github.com/alex/mediad/internal/config"
	"github.com/alex/mediad/internal/request"
	"github.com/alex/mediad/internal/storage"
	"github.com/alex/mediad/internal/tmdb"
)

// Module wires the HTTP server into the fx graph.
var Module = fx.Module("web",
	fx.Provide(New),
	fx.Invoke(registerServer),
)

// Server renders the interface and handles its forms.
type Server struct {
	cfg   *config.Config
	store *storage.Store
	tmdb  *tmdb.Client
	req   *request.Service
	tmpl  *templates
	log   *slog.Logger

	// version is shown in the footer, so that "which build is this" has an
	// answer without shelling into the host.
	version string
}

// New builds the server.
func New(
	cfg *config.Config,
	store *storage.Store,
	client *tmdb.Client,
	req *request.Service,
	log *slog.Logger,
) (*Server, error) {
	tmpl, err := newTemplates()
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:   cfg,
		store: store,
		tmdb:  client,
		req:   req,
		tmpl:  tmpl,
		log:   logger(log),
	}, nil
}

// Version is set by the command that builds the server, because the version
// string is injected into main at link time and this package cannot see it.
func (s *Server) SetVersion(v string) { s.version = v }

// routes builds the mux.
func (s *Server) routes() http.Handler {
	r := chi.NewRouter()

	r.Use(s.logRequests)
	r.Use(s.sessions)

	r.Handle("/static/*", s.staticHandler())

	// Reachable without a session, and each of them re-checks its own
	// precondition rather than trusting that it was routed here.
	r.Get("/login", s.handleLoginForm)
	r.Post("/login", s.handleLogin)
	r.Get("/setup", s.handleSetupForm)
	r.Post("/setup", s.handleSetup)
	r.Get("/join", s.handleJoinForm)
	r.Post("/join", s.handleJoin)
	r.Post("/logout", s.handleLogout)

	// Everything else needs a session. One group with one middleware, so that
	// a route added later is authenticated by default rather than by whoever
	// remembers to wrap it.
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)

		r.Get("/", s.handleDashboard)
		r.Get("/fragments/active", s.handleActiveFragment)

		// Posters are fetched by us, through the TMDB proxy. Behind a session
		// like everything else: an open image proxy is a way to make our
		// server fetch arbitrary TMDB images on demand, and the browser has
		// the cookie anyway.
		//
		// The trailing "*" is chi's wildcard, not "{path...}" — the latter is
		// a different router's syntax, and chi answers 404 for a nested path
		// under it. The captured value arrives as PathValue("*") and carries
		// no leading slash.
		r.Get("/poster/{size}/*", s.handlePoster)

		r.Get("/search", s.handleSearch)

		r.Get("/add/{kind}/{id}", s.handleAdd)
		r.Get("/add/{kind}/{id}/{season}", s.handleAddSeason)

		r.Post("/requests", s.handleCreateRequest)
		r.Get("/requests", s.handleRequests)
		r.Get("/requests/{id}", s.handleRequest)
		r.Post("/requests/{id}/cancel", s.handleCancel)
		r.Post("/requests/{id}/retry", s.handleRetry)

		r.Get("/notifications", s.handleNotifications)
		r.Post("/notifications/read", s.handleNotificationsRead)

		r.Get("/settings", s.handleSettings)
		r.Post("/settings/password", s.handlePassword)
		r.Post("/settings/invites", s.handleCreateInvite)
	})

	return r
}

// registerServer ties the HTTP server to the application lifecycle.
func registerServer(lc fx.Lifecycle, s *Server) {
	srv := &http.Server{
		Addr:    s.cfg.Server.Listen,
		Handler: s.routes(),

		// Bounded, because every one of them is a place a broken client can
		// hold a connection: the read timeout stops a slow request body, the
		// write timeout stops a slow reader, and the idle timeout reclaims the
		// keep-alive connections browsers leave open.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,

		ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			// Captured here rather than checked on every request: the listen
			// address is fixed at startup, so whether it is reachable from
			// anywhere but this host is a property of the deployment, not of a
			// request.
			if err := s.warnAboutExposure(); err != nil {
				return err
			}

			go func() {
				s.log.Info("http server listening", "addr", s.cfg.Server.Listen)
				if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					s.log.Error("http server stopped", "error", err)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			// Shutdown rather than Close: in-flight requests get until the
			// context expires to finish, which is what makes a restart not
			// drop somebody's form submission.
			return srv.Shutdown(ctx)
		},
	})
}

// warnAboutExposure says so when the interface is reachable from the network
// over plain HTTP.
//
// It is a warning rather than a refusal because binding to 0.0.0.0 behind a
// reverse proxy that terminates TLS is a normal setup, and refusing outright
// would break it. What is not normal is doing it without meaning to, and no
// other part of this project will mention it.
func (s *Server) warnAboutExposure() error {
	host, _, err := splitHostPort(s.cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("server.listen %q: %w", s.cfg.Server.Listen, err)
	}

	if !isLoopback(host) && !isTLS(s.cfg.Server.BaseURL) {
		s.log.Warn("the web interface is reachable from the network over plain HTTP; "+
			"passwords and session cookies will cross it in the clear",
			"listen", s.cfg.Server.Listen,
			"base_url", s.cfg.Server.BaseURL)
	}
	return nil
}
