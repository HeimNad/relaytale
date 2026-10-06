package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"relaytale/examples/website/site"
	"relaytale/internal/database"
	"relaytale/internal/webhook"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := database.Open(ctx, os.Getenv("DEMO_DATABASE_URL"))
	if err != nil {
		log.Fatal("demo database unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(5)
	var keys []webhook.Key
	if json.Unmarshal([]byte(os.Getenv("DEMO_WEBHOOK_KEYS")), &keys) != nil {
		log.Fatal("demo verification keys required")
	}
	app, err := site.New(ctx, site.Config{DB: db, GatewayURL: os.Getenv("DEMO_GATEWAY_URL"), GatewayToken: os.Getenv("DEMO_GATEWAY_TOKEN"), DemoToken: os.Getenv("DEMO_TOKEN"), Keys: keys})
	if err != nil {
		log.Fatal("demo configuration unavailable")
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				work, cancel := context.WithTimeout(ctx, 15*time.Second)
				if app.ReconcilePending(work) != nil {
					log.Print("status reconciliation pending; will retry")
				}
				cancel()
			}
		}
	}()
	server := &http.Server{Addr: "127.0.0.1:8090", Handler: app, ReadTimeout: 15 * time.Second, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Print("sandbox reference backend listening on 127.0.0.1:8090")
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal("demo server unavailable")
	}
}
