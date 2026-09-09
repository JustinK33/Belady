// Command registry serves the versioned model store. See internal/registry.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	beladyv1 "github.com/JustinK33/newproj/gen/belady/v1"
	"github.com/JustinK33/newproj/internal/config"
	"github.com/JustinK33/newproj/internal/grpcx"
	"github.com/JustinK33/newproj/internal/obs"
	"github.com/JustinK33/newproj/internal/registry"
)

func main() {
	log := obs.Init("registry")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dir := config.String("MODEL_DIR", "/var/lib/belady/models")
	srv, err := registry.New(dir)
	if err != nil {
		log.Error("model store init failed", "dir", dir, "err", err)
		os.Exit(2)
	}
	log.Info("registry configured", "dir", dir, "latest", srv.Latest())

	go func() {
		if err := obs.Serve(ctx, config.String("DEBUG_ADDR", ":9090")); err != nil {
			log.Error("debug endpoint failed", "err", err)
		}
	}()

	g := grpcx.NewServer()
	beladyv1.RegisterRegistryServer(g, srv)
	if err := grpcx.Serve(ctx, config.String("GRPC_ADDR", ":8082"), g); err != nil {
		log.Error("serve failed", "err", err)
	}
}
