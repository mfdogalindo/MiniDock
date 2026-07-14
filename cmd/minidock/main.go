package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/julieta/minidock/internal/app"
	"github.com/julieta/minidock/internal/store"
)

func main() {
	config := app.LoadConfig()
	database, err := store.Open(config.DatabasePath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()

	application, err := app.New(config, database)
	if err != nil {
		log.Fatalf("create application: %v", err)
	}
	defer application.Lock()

	server := &http.Server{
		Addr:              config.Address,
		Handler:           application.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errors := make(chan error, 1)
	go func() {
		log.Printf("MiniDock listening on %s", config.Address)
		errors <- server.ListenAndServe()
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errors:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	case <-shutdown:
		log.Print("shutting down")
	}

	context, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(context); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
