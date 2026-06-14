// Command poller serves the WAL/buffer visualizer frontend and, when a
// PostgreSQL DSN is configured, streams live frames sampled from that instance.
// Without a DSN it still serves the frontend and teaching mode (which is fully
// client-side), so the tool is useful with no database at all.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/catherinethompson/pg-sys-models/internal/config"
	"github.com/catherinethompson/pg-sys-models/internal/db"
	"github.com/catherinethompson/pg-sys-models/internal/poller"
	"github.com/catherinethompson/pg-sys-models/internal/server"
)

func main() {
	cfg := config.Load()
	webDir := getenv("WEB_DIR", "web")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := server.New(webDir)

	if cfg.DSN == "" {
		log.Printf("no PG_DSN set: serving frontend + teaching mode only (live mode disabled)")
	} else {
		// Live mode. A failure here is non-fatal: the frontend and teaching
		// mode stay available (spec §16 graceful degradation).
		connCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		d, err := db.Open(connCtx, cfg.DSN, cfg.AppName, cfg.StatementTimeout)
		cancel()
		if err != nil {
			log.Printf("live mode unavailable: %v", err)
		} else {
			defer d.Close()
			caps := d.Caps()
			log.Printf("connected: server_version_num=%d pg_stat_io=%v pg_buffercache=%v",
				caps.ServerVersionNum, caps.HasStatIO, caps.HasBuffercache)
			p := poller.New(cfg, d)
			go srv.Pump(p.Frames())
			go p.Run(ctx)
		}
	}

	httpSrv := &http.Server{Addr: cfg.HTTPAddr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("listening on %s (web dir %q)", cfg.HTTPAddr, webDir)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server: %v", err)
	}
}

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}
