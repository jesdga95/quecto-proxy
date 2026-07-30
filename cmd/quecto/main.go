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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	adminSrv.Routes(mux)
	mux.Handle("/v1/", px.Handler())

	dashboard, err := web.Handler()
	if err != nil {
		return err
	}
	mux.Handle(web.Prefix+"/", dashboard)
	if !web.Built() {
		log.Warn("the dashboard was not built into this binary, admin pages are unavailable",
			"fix", "make dashboard && go build ./cmd/quecto")
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, web.Prefix+"/", http.StatusFound)
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go writer.Run(ctx)
	go sweepIdleKeys(ctx, limiter)

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Listen, "upstream", live.Values().UpstreamURL, "keys", registry.Len())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)

	writer.Wait()
	saveBalances(db, limiter, log)
	return shutdownErr
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
