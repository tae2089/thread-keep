package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/coordinator/app"
)

func TestCoordinatorCommandExposesCanonicalFlags(t *testing.T) {
	var stderr bytes.Buffer
	err := run([]string{"-h"}, &stderr)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("run(-h) error = %v, want flag.ErrHelp", err)
	}
	help := stderr.String()
	for _, name := range []string{"coordinator-id", "runner-path"} {
		if !strings.Contains(help, name) {
			t.Fatalf("coordinator help missing %q:\n%s", name, help)
		}
	}
	for _, stale := range []string{"planner child", "planner-worker", "runner-id", "thread-keep-planner-runner"} {
		if strings.Contains(help, stale) {
			t.Fatalf("coordinator help contains stale component term %q:\n%s", stale, help)
		}
	}
}

func TestLoadConfigDecodesCoordinatorRunnerBlockWithoutChangingServerConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	contents, err := json.Marshal(map[string]any{
		"github_api_base_url": "https://api.github.invalid",
		"repositories":        map[string]any{},
		"runner": map[string]any{
			"backend":         "process",
			"timeout_seconds": 41,
			"process":         map[string]any{"path": "/configured/runner"},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	serverConfig, runnerConfig, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if serverConfig.GitHubAPIBaseURL != "https://api.github.invalid" || runnerConfig.Backend != app.BackendProcess || runnerConfig.TimeoutSeconds != 41 || runnerConfig.Process.Path != "/configured/runner" {
		t.Fatalf("loadConfig() = server %+v, runner %+v", serverConfig, runnerConfig)
	}
}

func TestRunnerOverridesIncludeOnlyExplicitFlags(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	path := flags.String("runner-path", "thread-keep-runner", "")
	timeout := flags.Duration("executor-timeout", 2*time.Minute, "")
	if err := flags.Parse(nil); err != nil {
		t.Fatalf("Parse(defaults) error = %v", err)
	}
	overrides := runnerOverrides(flags, *path, *timeout)
	if overrides.ProcessPath != nil || overrides.Timeout != nil {
		t.Fatalf("runnerOverrides(defaults) = %+v", overrides)
	}

	flags = flag.NewFlagSet("test", flag.ContinueOnError)
	path = flags.String("runner-path", "thread-keep-runner", "")
	timeout = flags.Duration("executor-timeout", 2*time.Minute, "")
	if err := flags.Parse([]string{"--runner-path=/explicit/runner", "--executor-timeout=53s"}); err != nil {
		t.Fatalf("Parse(explicit) error = %v", err)
	}
	overrides = runnerOverrides(flags, *path, *timeout)
	if overrides.ProcessPath == nil || *overrides.ProcessPath != "/explicit/runner" || overrides.Timeout == nil || *overrides.Timeout != 53*time.Second {
		t.Fatalf("runnerOverrides(explicit) = %+v", overrides)
	}
}
