package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type config struct {
	server string
	repo   string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("thread-keep-e2e-mcpclient", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configuration := config{}
	flags.StringVar(&configuration.server, "server", "thread-keep-mcp", "path to the MCP server binary")
	flags.StringVar(&configuration.repo, "repo", "", "path to the Git worktree exposed by the server")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if configuration.repo == "" {
		return errors.New("--repo is required")
	}

	ctx := context.Background()
	command := exec.CommandContext(ctx, configuration.server)
	command.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "thread-keep-e2e", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		return fmt.Errorf("connect to MCP server: %w", err)
	}
	defer func() { _ = session.Close() }()
	if err := writeJSON(session.InitializeResult()); err != nil {
		return err
	}

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	if err := writeJSON(tools); err != nil {
		return err
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "note_add",
		Arguments: map[string]any{
			"repo":       configuration.repo,
			"entity_key": "sample.Run",
			"kind":       "intent",
			"body":       "drafted through mcp",
		},
	})
	if err != nil {
		return fmt.Errorf("call note_add: %w", err)
	}
	if err := writeJSON(result); err != nil {
		return err
	}
	if result.IsError {
		return errors.New("note_add returned a tool error")
	}
	if err := session.Close(); err != nil {
		return fmt.Errorf("close MCP session: %w", err)
	}
	return nil
}

func writeJSON(value any) error {
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		return fmt.Errorf("encode MCP result: %w", err)
	}
	return nil
}
