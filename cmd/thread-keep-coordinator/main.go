package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tae2089/thread-keep/internal/coordinator/app"
	"github.com/tae2089/thread-keep/internal/coordinator/runtime"
	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote/server"
)

const (
	defaultGitHubAPIBaseURL         = "https://api.github.com"
	githubPrivateKeyFileEnvironment = "THREAD_KEEP_GITHUB_APP_PRIVATE_KEY_FILE"
)

type coordinatorConfigEnvelope struct {
	Runner app.RunnerConfig `json:"runner"`
}

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(arguments []string, stderr io.Writer) error {
	defaults := runtime.DefaultConfig()
	flags := flag.NewFlagSet("thread-keep-coordinator", flag.ContinueOnError)
	flags.SetOutput(stderr)
	storage := flags.String("storage", "", "absolute path for shared context object storage")
	configPath := flags.String("config", "", "path to the repository mapping configuration file")
	databaseDSN := flags.String("db-dsn", "", "shared PostgreSQL DSN or embedded database path")
	runnerPath := flags.String("runner-path", "thread-keep-runner", "one-job runner executable")
	coordinatorID := flags.String("coordinator-id", "thread-keep-coordinator", "coordinator display name stored with the singleton lease and used in startup logs")
	mode := flags.String("mode", defaults.Mode, "coordinator mode: durable_single")
	replicas := flags.Int("replicas", defaults.Replicas, "declared coordinator replica count")
	workers := flags.Int("workers", defaults.Workers, "coordinator planning worker count")
	pollInterval := flags.Duration("poll-interval", defaults.PollInterval, "durable job poll interval")
	executorTimeout := flags.Duration("executor-timeout", defaults.ExecutorTimeout, "runner process timeout")
	finalizeMargin := flags.Duration("finalize-margin", defaults.FinalizeMargin, "post-executor finalize margin")
	jobTimeout := flags.Duration("job-timeout", defaults.JobTimeout, "total job timeout")
	leaseDuration := flags.Duration("lease-duration", defaults.LeaseDuration, "job lease duration")
	shutdownGrace := flags.Duration("shutdown-grace", defaults.ShutdownGrace, "graceful shutdown bound")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *storage == "" || *configPath == "" || *databaseDSN == "" {
		return errors.New("--storage, --config, and --db-dsn are required")
	}
	config, configuredRunner, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	runnerConfig, err := app.ResolveRunnerConfig(configuredRunner, app.RunnerDefaults{ProcessPath: "thread-keep-runner", Timeout: defaults.ExecutorTimeout}, runnerOverrides(flags, *runnerPath, *executorTimeout))
	if err != nil {
		return err
	}
	storageRoot, err := filepath.Abs(*storage)
	if err != nil {
		return fmt.Errorf("resolve storage path: %w", err)
	}
	objects, err := server.OpenStorage(storageRoot, *databaseDSN)
	if err != nil {
		return err
	}
	defer objects.Close()
	privateKeyFile := os.Getenv(githubPrivateKeyFileEnvironment)
	if privateKeyFile == "" {
		return errors.New("coordinator requires THREAD_KEEP_GITHUB_APP_PRIVATE_KEY_FILE")
	}
	privateKey, err := os.ReadFile(privateKeyFile)
	if err != nil {
		return fmt.Errorf("read GitHub App private key file: %w", err)
	}
	defer clear(privateKey)
	coordinator, err := app.BuildCoordinator(config, objects.RefStore(), objects, runnerConfig, privateKey)
	if err != nil {
		return err
	}
	runtimeConfig := defaults
	runtimeConfig.Mode = *mode
	runtimeConfig.Replicas = *replicas
	runtimeConfig.Workers = *workers
	runtimeConfig.PollInterval = *pollInterval
	runtimeConfig.ExecutorTimeout = runnerConfig.Timeout
	runtimeConfig.FinalizeMargin = *finalizeMargin
	runtimeConfig.JobTimeout = *jobTimeout
	runtimeConfig.LeaseDuration = *leaseDuration
	runtimeConfig.ShutdownGrace = *shutdownGrace
	runtimeConfig.ReconcileInterval = time.Duration(runnerConfig.ReconcileIntervalSeconds) * time.Second
	runtimeConfig.OnError = func(err error) { fmt.Fprintf(stderr, "coordinator job error code=%s\n", domain.CodeOf(err)) }
	coordinatorProcess, err := runtime.NewGuarded(runtimeConfig, coordinator, objects.RefStore())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(stderr, "thread-keep-coordinator started id=%s workers=%d mode=%s\n", *coordinatorID, runtimeConfig.Workers, runtimeConfig.Mode)
	return coordinatorProcess.Run(ctx, *coordinatorID)
}

func loadConfig(path string) (server.Config, app.RunnerConfig, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return server.Config{}, app.RunnerConfig{}, fmt.Errorf("read configuration: %w", err)
	}
	var config server.Config
	if err := json.Unmarshal(contents, &config); err != nil {
		return server.Config{}, app.RunnerConfig{}, fmt.Errorf("decode configuration: %w", err)
	}
	var envelope coordinatorConfigEnvelope
	if err := json.Unmarshal(contents, &envelope); err != nil {
		return server.Config{}, app.RunnerConfig{}, fmt.Errorf("decode runner configuration: %w", err)
	}
	if config.GitHubAPIBaseURL == "" {
		config.GitHubAPIBaseURL = defaultGitHubAPIBaseURL
	}
	return config, envelope.Runner, nil
}

func runnerOverrides(flags *flag.FlagSet, runnerPath string, executorTimeout time.Duration) app.RunnerOverrides {
	visited := make(map[string]bool)
	flags.Visit(func(item *flag.Flag) { visited[item.Name] = true })
	overrides := app.RunnerOverrides{}
	if visited["runner-path"] {
		overrides.ProcessPath = &runnerPath
	}
	if visited["executor-timeout"] {
		overrides.Timeout = &executorTimeout
	}
	return overrides
}
