package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	repo := flags.String("repo", "", "default Git worktree path when a tool call omits repo")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	ctx := context.Background()
	fmt.Fprintln(os.Stderr, "thread-keep-mcp listening on stdio")
	return mcpserver.New(*repo).Run(ctx, &mcp.StdioTransport{})
}
