package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/tae2089/thread-keep/internal/remote/server"
)

const (
	defaultGitHubAPIBaseURL = "https://api.github.com"
	serverReadHeaderTimeout = 5 * time.Second
	serverReadTimeout       = 15 * time.Minute
	serverWriteTimeout      = 15 * time.Minute
	serverIdleTimeout       = 60 * time.Second
	serverMaxHeaderBytes    = 64 << 10
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdout io.Writer, stderr *os.File) error {
	flags := flag.NewFlagSet("thread-keep-server", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", "127.0.0.1:8320", "listen address")
	storage := flags.String("storage", "", "absolute path for context object storage")
	configPath := flags.String("config", "", "path to the repository mapping configuration file")
	refDatabaseDSN := flags.String("db-dsn", "", "ref database DSN: postgres://... or an embedded database file path (default <storage>/refs.db)")
	gcMode := flags.Bool("gc", false, "run one garbage-collection pass and exit without serving")
	gcGrace := flags.Duration("gc-grace", 14*24*time.Hour, "objects newer than this age are never collected")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *storage == "" || *configPath == "" {
		return fmt.Errorf("both --storage and --config are required")
	}
	storageRoot, err := filepath.Abs(*storage)
	if err != nil {
		return fmt.Errorf("resolve storage path: %w", err)
	}
	if err := os.MkdirAll(storageRoot, 0o755); err != nil {
		return fmt.Errorf("create storage root: %w", err)
	}
	contents, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	var config server.Config
	if err := json.Unmarshal(contents, &config); err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	if config.GitHubAPIBaseURL == "" {
		config.GitHubAPIBaseURL = defaultGitHubAPIBaseURL
	}
	store, err := server.OpenStorage(storageRoot, *refDatabaseDSN)
	if err != nil {
		return err
	}
	defer store.Close()
	if *gcMode {
		repositories := make([]string, 0, len(config.Repositories))
		for repositoryID := range config.Repositories {
			repositories = append(repositories, repositoryID)
		}
		sort.Strings(repositories)
		result, err := server.RunGC(context.Background(), store, repositories, *gcGrace)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(result)
	}
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	background, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
	policy, err := server.ResolveMaintenancePolicy(config.GC)
	if err != nil {
		return err
	}
	if policy.Interval > 0 {
		repositories := make([]string, 0, len(config.Repositories))
		for repositoryID := range config.Repositories {
			repositories = append(repositories, repositoryID)
		}
		sort.Strings(repositories)
		go server.RunPeriodicGC(background, store, repositories, policy.Grace, policy.Interval)
	}
	var handler http.Handler
	var clusterRuntime *server.ClusterRuntime
	var clusterSecret string
	var serving server.Storage = store
	leave := func(context.Context) error { return nil }
	if config.Cluster != nil {
		clusterSecret = os.Getenv("THREAD_KEEP_CLUSTER_SECRET")
		if clusterSecret == "" {
			return fmt.Errorf("cluster mode requires the THREAD_KEEP_CLUSTER_SECRET environment variable")
		}
		runtime, err := server.NewClusterRuntime(store, config, clusterSecret)
		if err != nil {
			return err
		}
		clusterRuntime = runtime
		serving = runtime.Storage
		if err := runtime.Membership.Start(background); err != nil {
			return err
		}
		leave = runtime.Membership.Leave
		go runtime.AntiEntropy.Run(background, runtime.AntiEntropyInterval)
	} else {
		if policy.Auto {
			serving = server.NewMaintainer(store, policy).Wrap(store)
		}
	}
	coordinator, err := buildCoordinator(config, store.RefStore(), serving)
	if err != nil {
		return err
	}
	if clusterRuntime != nil {
		if coordinator == nil {
			handler = clusterRuntime.Handler
		} else {
			handler, err = server.NewClusterCoordinatorHandler(store, serving, clusterSecret, coordinator, config)
		}
	} else if coordinator != nil {
		handler, err = server.NewCoordinatorHandler(serving, coordinator, config)
	} else {
		handler, err = server.NewHandler(serving, config)
	}
	if err != nil {
		return err
	}
	handler, err = buildPublicHandler(config, store.RefStore(), handler)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	fmt.Fprintf(stderr, "thread-keep-server listening on %s\n", listener.Addr())
	return serveUntilShutdown(signalCtx, listener, handler, leave, stderr)
}

func serveUntilShutdown(ctx context.Context, listener net.Listener, handler http.Handler, leave func(context.Context) error, stderr io.Writer) error {
	server := newHTTPServer(handler)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	fmt.Fprintln(stderr, "shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if err := leave(shutdownCtx); err != nil && shutdownErr == nil {
		shutdownErr = err
	}
	return shutdownErr
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    serverMaxHeaderBytes,
	}
}
