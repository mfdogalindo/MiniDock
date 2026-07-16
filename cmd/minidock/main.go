package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/julieta/minidock/internal/agent"
	"github.com/julieta/minidock/internal/app"
	"github.com/julieta/minidock/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	if len(os.Args) > 1 {
		if err := backupCommand(os.Args[1:], os.Stdin); err != nil {
			log.Fatal(err)
		}
		return
	}
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
	var agentServer *grpc.Server
	if config.AgentAddress != "" {
		tlsConfig, err := agent.ServerTLSConfig(config.AgentTLSCertificatePath, config.AgentTLSPrivateKeyPath, config.AgentTLSClientCAPath)
		if err != nil {
			log.Fatalf("configure agent control plane: %v", err)
		}
		listener, err := net.Listen("tcp", config.AgentAddress)
		if err != nil {
			log.Fatalf("listen for agents: %v", err)
		}
		agent.RegisterCodec()
		agentServer = grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
		agent.RegisterControlServer(agentServer, agent.ControlPlane{Store: database})
		go func() {
			if err := agentServer.Serve(listener); err != nil {
				log.Printf("agent control plane stopped: %v", err)
			}
		}()
		log.Printf("agent control plane listening on %s", config.AgentAddress)
	}
	defer func() {
		if agentServer != nil {
			agentServer.GracefulStop()
		}
	}()
	queueContext, stopQueue := context.WithCancel(context.Background())
	defer stopQueue()
	if err := application.StartQueue(queueContext); err != nil {
		log.Fatalf("start deployment queue: %v", err)
	}

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
