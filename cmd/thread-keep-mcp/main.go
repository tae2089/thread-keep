package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tae2089/thread-keep/internal/app"
	"github.com/tae2089/thread-keep/internal/mcpserver"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("thread-keep-mcp", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repo := flags.String("repo", ".", "path to the Git worktree whose context this server exposes")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	ctx := context.Background()
	service, err := app.Open(ctx, *repo)
	if err != nil {
		return err
	}
	defer service.Close()
	fmt.Fprintln(os.Stderr, "thread-keep-mcp listening on stdio")
	return mcpserver.New(service).Run(ctx, &mcp.StdioTransport{})
}
