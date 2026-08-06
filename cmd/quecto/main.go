package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"quecto/internal/admin"
	"quecto/internal/config"
	"quecto/internal/keys"
	"quecto/internal/limit"
	"quecto/internal/metrics"
	"quecto/internal/proxy"
	"quecto/internal/settings"
	"quecto/internal/store"
	"quecto/internal/usage"
	"quecto/internal/web"
)

const idleKeySweep = 30 * time.Minute

// healthPath is Quecto's own liveness route, sharing the /quecto namespace with
// the admin API. It answers for this process only. Whether the model is loaded
// is the upstream's own /health, which is forwarded and needs a key.
const healthPath = "/quecto/health"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if err := run(log); err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	live, err := settings.Load(cfg, db)
	if err != nil {
		return err
	}
	registry, err := keys.NewRegistry(db)
	if err != nil {
		return err
	}

	limiter := limit.New()
	restoreBalances(db, registry, limiter, log)

	gate := metrics.NewHistogram()
	px := proxy.New(live, registry, limiter, proxy.Options{
		HeaderTimeout:    cfg.UpstreamHeaderTimeout,
		TrustForwardedIP: cfg.TrustProxyHeaders,
	}, log)
	px.SetObserver(gate)

	writer := usage.NewWriter(db, live, log)
	px.SetReporter(writer)

	adminSrv, err := admin.New(cfg, db, registry, limiter, live, newHealth(px, writer, live, gate))
	if err != nil {
		return err
	}

	dashboard, err := web.Handler(adminSrv.Authenticated)
	if err != nil {
		return err
	}
	if !web.Built() {
		log.Warn("the dashboard was not built into this binary, admin pages are unavailable",
			"fix", "make dashboard && go build ./cmd/quecto")
	}
	public, adminMux := routes(cfg, adminSrv.Handler(), dashboard, px.Handler())

	servers := []*http.Server{{
		Addr:              cfg.Listen,
		Handler:           public,
		ReadHeaderTimeout: 10 * time.Second,
	}}
	if cfg.AdminListen != cfg.Listen {
		servers = append(servers, &http.Server{
			Addr:              cfg.AdminListen,
			Handler:           adminMux,
			ReadHeaderTimeout: 10 * time.Second,
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go writer.Run(ctx)
	go sweepIdleKeys(ctx, limiter)

	log.Info("listening", "addr", cfg.Listen, "upstream", live.Values().UpstreamURL, "keys", registry.Len())
	log.Info("dashboard listening", "addr", cfg.AdminListen+web.Prefix)

	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var shutdownErr error
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil && shutdownErr == nil {
			shutdownErr = err
		}
	}

	writer.Wait()
	saveBalances(db, limiter, log)
	return shutdownErr
}

// routes mounts each handler on the port it belongs to. Every mount owns its
// own surface end to end, including how it answers a path it does not serve,
// so nothing here needs to know a route or an error shape.
//
// The two muxes are the same one unless the dashboard was given its own
// address, in which case the proxy port carries no admin URL at all.
func routes(cfg *config.Config, adminAPI, dashboard, px http.Handler) (proxyMux, adminMux *http.ServeMux) {
	proxyMux = http.NewServeMux()
	proxyMux.HandleFunc("GET "+healthPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	proxyMux.Handle("/v1/", px)
	// The upstream serves these at its root rather than under /v1, so each is
	// mounted by name and method. Naming them is what keeps everything else the
	// model server exposes, generating or state changing, off this port.
	for _, rt := range proxy.UtilityRoutes {
		proxyMux.Handle(rt.Method+" "+rt.Path, px)
	}
	proxyMux.Handle("/", proxy.NotFoundHandler())

	adminMux = proxyMux
	if cfg.AdminListen != cfg.Listen {
		adminMux = http.NewServeMux()
		// This port is opened in a browser, so what it says about a path it
		// does not serve is a page. Sharing a port with the proxy, that one is
		// already answering here and claims the pattern.
		adminMux.Handle("/", web.NotFoundHandler())
	}
	adminMux.Handle(admin.Prefix+"/", adminAPI)
	adminMux.Handle(web.Prefix+"/", dashboard)
	adminMux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, web.Prefix+"/", http.StatusFound)
	})
	return proxyMux, adminMux
}

func sweepIdleKeys(ctx context.Context, limiter *limit.Limiter) {
	t := time.NewTicker(idleKeySweep)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			limiter.Sweep(idleKeySweep)
		case <-ctx.Done():
			return
		}
	}
}

// restoreBalances reloads token balances so a restart is not a free reset.
func restoreBalances(db *store.Store, registry *keys.Registry, limiter *limit.Limiter, log *slog.Logger) {
	saved, err := db.LoadLimiterState()
	if err != nil {
		log.Warn("could not restore token balances", "error", err)
		return
	}
	for _, k := range registry.All() {
		if balance, ok := saved[k.ID]; ok {
			limiter.Restore(k, balance)
		}
	}
}

func saveBalances(db *store.Store, limiter *limit.Limiter, log *slog.Logger) {
	if err := db.SaveLimiterState(limiter.Balances(), time.Now().Unix()); err != nil {
		log.Warn("could not save token balances", "error", err)
	}
}
