package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"pgadmin-manager/internal/dashboard"
)

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.LUTC)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	service, err := dashboard.NewPostgresService(ctx)
	if err != nil {
		logger.Fatalf("initialize PostgreSQL: %v", err)
	}
	defer service.Close()

	handler, err := dashboard.NewHandler(service, logger)
	if err != nil {
		logger.Fatalf("initialize web server: %v", err)
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Printf("listening on %s", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Printf("shutdown: %v", err)
	}
}
