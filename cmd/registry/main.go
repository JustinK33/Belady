// Command registry serves the versioned model store. See internal/registry.
package main

import (
	beladyv1 "github.com/JustinK33/Belady/gen/belady/v1"
	"github.com/JustinK33/Belady/internal/config"
	"github.com/JustinK33/Belady/internal/grpcx"
	"github.com/JustinK33/Belady/internal/obs"
	"github.com/JustinK33/Belady/internal/registry"
)

func main() {
	log, ctx, stop := obs.Start("registry")
	defer stop()

	dir := config.String("MODEL_DIR", "/var/lib/belady/models")
	srv, err := registry.New(dir)
	if err != nil {
		obs.Fatal(log, "model store init failed", "dir", dir, "err", err)
	}
	log.Info("registry configured", "dir", dir, "latest", srv.Latest())

	g := grpcx.NewServer()
	beladyv1.RegisterRegistryServer(g, srv)
	if err := grpcx.Serve(ctx, config.String("GRPC_ADDR", ":8082"), g); err != nil {
		log.Error("serve failed", "err", err)
	}
}
