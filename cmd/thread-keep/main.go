package main

import (
	"context"
	"os"

	"github.com/tae2089/thread-keep/internal/app"
	"github.com/tae2089/thread-keep/internal/cli"
)

func main() {
	runner := cli.NewRunner(app.Open)
	os.Exit(runner.Execute(context.Background(), NewRoot(runner), os.Args[1:], os.Stdout, os.Stderr))
}
