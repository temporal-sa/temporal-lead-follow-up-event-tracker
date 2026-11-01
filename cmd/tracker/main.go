package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/store"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/web"
	enums "go.temporal.io/api/enums/v1"
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
	if mode != "serve" && mode != "worker" && mode != "dev" && mode != "backfill" {
		return errors.New("usage: tracker [serve|worker|dev|backfill [--verify-only]]")
	}
	writesPaused, err := strconv.ParseBool(env("WRITES_PAUSED", "false"))
	if err != nil {
		return errors.New("WRITES_PAUSED must be true or false")
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
	if mode == "backfill" {
		return backfill(ctx, c, taskQueue, os.Args[2:])
	}
	if mode == "worker" || mode == "dev" {
		archive, err := store.Open(ctx, store.Config{Table: os.Getenv("EVENT_LEADS_TABLE"), Endpoint: os.Getenv("DYNAMODB_ENDPOINT")})
		if err != nil {
			return err
		}
		if os.Getenv("DYNAMODB_ENDPOINT") != "" {
			if err := archive.EnsureTable(ctx); err != nil {
				return fmt.Errorf("create the local DynamoDB table: %w", err)
			}
		}
		if writesPaused {
			tracker.PauseWrites()
		}
		// The employee event list queries a page of events at once, and each query is a workflow task.
		options := worker.Options{WorkflowTaskPollerBehavior: worker.NewPollerBehaviorAutoscaling(worker.PollerBehaviorAutoscalingOptions{InitialNumberOfPollers: 10, MinimumNumberOfPollers: 10})}
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
		w.RegisterWorkflow(tracker.EventAliasWorkflow)
		w.RegisterWorkflow(tracker.FlightPassWorkflow)
		w.RegisterWorkflow(tracker.BackfillWorkflow)
		w.RegisterActivity(&tracker.Activities{Client: c, Archive: archive})
		tracker.Service{Client: c, TaskQueue: taskQueue}.Register(w)
		if err := w.Start(); err != nil {
			return err
		}
		defer w.Stop()
		slog.Info("worker started", "taskQueue", taskQueue, "deployment", deployment, "build", build, "writesPaused", writesPaused)
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
		WritesPaused:  writesPaused,
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

// backfill copies every event into the archive, or with --verify-only checks the
// archive against the workflows, and prints the report. A worker with the archive
// configured runs it.
func backfill(ctx context.Context, c client.Client, taskQueue string, args []string) error {
	flags := flag.NewFlagSet("backfill", flag.ContinueOnError)
	verifyOnly := flags.Bool("verify-only", false, "compare the archive with the workflows without writing")
	if err := flags.Parse(args); err != nil {
		return err
	}
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: tracker.BackfillWorkflowID, TaskQueue: taskQueue,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		// Fail rather than attach: a run already going may be in the other mode.
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, tracker.BackfillWorkflow, tracker.BackfillInput{VerifyOnly: *verifyOnly})
	if err != nil {
		return fmt.Errorf("start the backfill: %w", err)
	}
	var report tracker.BackfillReport
	if err := run.Get(ctx, &report); err != nil {
		return fmt.Errorf("backfill %s failed: %w", run.GetRunID(), err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if len(report.Problems) > 0 {
		return fmt.Errorf("%d events failed or don't match the archive", len(report.Problems))
	}
	return nil
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
