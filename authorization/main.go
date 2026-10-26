package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func required(key string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Fatalf("%s is required", key)
	}
	return value
}
func main() {
	if len(os.Args) > 1 && os.Args[1] == "health" {
		client := http.Client{Timeout: 2 * time.Second}
		res, err := client.Get("http://localhost:8080/health")
		if err != nil {
			os.Exit(1)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	store, err := openStore(env("AUTHZ_DATABASE", "/data/permissions.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer store.DB.Close()
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err = store.Migrate(env("SEED_FILE", "/config/seed.json")); err != nil {
			log.Fatal(err)
		}
		log.Print("Database migrations complete")
		return
	}
	if err = store.Start(); err != nil {
		log.Fatal(err)
	}
	upstream, err := url.Parse(env("GATEWAY_UPSTREAM", "http://agentgateway:8080"))
	if err != nil {
		log.Fatal(err)
	}
	server := &Server{Store: store, Verifier: &Verifier{Issuer: required("OIDC_ISSUER"), Audience: required("OIDC_AUDIENCE"), JWKS: required("OIDC_JWKS_URL")}, Upstream: upstream, AdminToken: required("AUTHZ_ADMIN_TOKEN"), CheckToken: required("AUTHZ_CHECK_TOKEN")}
	internal := http.NewServeMux()
	internal.HandleFunc("POST /check", server.Check)
	servers := []*http.Server{
		{Addr: ":8080", Handler: server.PublicHandler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second},
		{Addr: ":8000", Handler: internal, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second},
	}
	for _, srv := range servers {
		go func(srv *http.Server) {
			log.Printf("Listening on %s", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal(err)
			}
		}(srv)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdown)
	}
}
