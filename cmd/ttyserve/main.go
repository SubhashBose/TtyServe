package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ttyserve/internal/auth"
	"ttyserve/internal/config"
	"ttyserve/internal/server"
	"ttyserve/internal/session"
)

func main() {
	cfgPath := flag.String("config", "", "path to YAML config file")
	listen := flag.String("listen", "", "override listen address (e.g. :7681)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	authn, err := auth.New(cfg)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	mgr := session.NewManager(cfg)
	defer mgr.Shutdown()

	srv, err := server.New(cfg, authn, mgr)
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	httpSrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
	}

	go func() {
		log.Printf("ttyserve listening on %s (persistence=%v mode=%s multi=%v)",
			cfg.Listen, cfg.SessionPersistence, cfg.PersistenceMode, cfg.MultiSession)
		var err error
		if cfg.TLSEnabled() {
			err = httpSrv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			err = httpSrv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	mgr.Shutdown()
}
