package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/web"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func main() {
	if err := run(); err != nil {
		slog.Error("tracker stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode != "serve" && mode != "worker" && mode != "dev" {
		return errors.New("usage: tracker [serve|worker|dev]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	options, err := temporalOptions()
	if err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	c, err := client.DialContext(dialCtx, options)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to Temporal: %w", err)
	}
	defer c.Close()
	taskQueue := env("TEMPORAL_TASK_QUEUE", tracker.DefaultTaskQueue)
	if mode == "worker" || mode == "dev" {
		options := worker.Options{}
		deployment, build := os.Getenv("TEMPORAL_DEPLOYMENT_NAME"), os.Getenv("TEMPORAL_WORKER_BUILD_ID")
		if deployment != "" || build != "" {
			if deployment == "" || build == "" {
				return errors.New("worker versioning requires both TEMPORAL_DEPLOYMENT_NAME and TEMPORAL_WORKER_BUILD_ID")
			}
			options.DeploymentOptions = worker.DeploymentOptions{UseVersioning: true, Version: worker.WorkerDeploymentVersion{DeploymentName: deployment, BuildID: build}, DefaultVersioningBehavior: workflow.VersioningBehaviorPinned}
		}
		w := worker.New(c, taskQueue, options)
		w.RegisterWorkflow(tracker.EventWorkflow)
		w.RegisterWorkflow(tracker.ParticipantShardWorkflow)
		w.RegisterWorkflow(tracker.FlightPassWorkflow)
		w.RegisterActivity(&tracker.Activities{Client: c})
		tracker.Service{Client: c, TaskQueue: taskQueue}.Register(w)
		if err := w.Start(); err != nil {
			return err
		}
		defer w.Stop()
		slog.Info("worker started", "taskQueue", taskQueue, "deployment", deployment, "build", build)
	}
	if mode == "worker" {
		<-ctx.Done()
		return nil
	}
	address := env("LISTEN_ADDRESS", "127.0.0.1:8080")
	devEmail := os.Getenv("DEV_AUTH_EMAIL")
	if devEmail != "" {
		if !strings.HasPrefix(address, "127.0.0.1:") && !strings.HasPrefix(address, "[::1]:") && !strings.HasPrefix(address, "localhost:") {
			return errors.New("DEV_AUTH_EMAIL requires a loopback LISTEN_ADDRESS")
		}
		slog.Warn("local development employee authentication enabled", "email", devEmail)
	}
	handler, err := web.New(web.Config{
		PublicURL:     env("PUBLIC_URL", "http://localhost:8080"),
		AuthBaseURL:   env("AUTH_BASE_URL", "https://catalog.tmprl-demo.cloud"),
		AuthVerifyURL: env("AUTH_VERIFY_URL", "https://catalog.tmprl-demo.cloud/_auth/verify"),
		DevAuthEmail:  devEmail,
	}, web.NewTemporalGateway(c, taskQueue, options.Namespace))
	if err != nil {
		return err
	}
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	slog.Info("web started", "address", address)
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func temporalOptions() (client.Options, error) {
	options := client.Options{HostPort: env("TEMPORAL_ADDRESS", "localhost:7233"), Namespace: env("TEMPORAL_NAMESPACE", "default")}
	key := os.Getenv("TEMPORAL_API_KEY")
	if key != "" {
		options.Credentials = client.NewAPIKeyStaticCredentials(key)
	}
	certPath, keyPath := os.Getenv("TEMPORAL_CLIENT_CERT"), os.Getenv("TEMPORAL_CLIENT_KEY")
	if certPath != "" || keyPath != "" {
		if certPath == "" || keyPath == "" {
			return options, errors.New("mTLS requires TEMPORAL_CLIENT_CERT and TEMPORAL_CLIENT_KEY file paths")
		}
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return options, fmt.Errorf("load Temporal client certificate: %w", err)
		}
		options.ConnectionOptions.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	} else if key != "" || strings.EqualFold(os.Getenv("TEMPORAL_TLS"), "true") {
		options.ConnectionOptions.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if path := os.Getenv("TEMPORAL_CA_CERT"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return options, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return options, errors.New("invalid Temporal CA certificate")
		}
		if options.ConnectionOptions.TLS == nil {
			options.ConnectionOptions.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		options.ConnectionOptions.TLS.RootCAs = pool
	}
	return options, nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
