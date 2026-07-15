package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tae2089/thread-keep/internal/planner"
	"github.com/tae2089/thread-keep/internal/runner/protocol"
)

func main() {
	if exitCode := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); exitCode != 0 {
		os.Exit(exitCode)
	}
}

func run(arguments []string, input io.Reader, output, stderr io.Writer) int {
	return runWithRunner(arguments, input, output, stderr, planner.NewNativeRunner(planner.NativeConfig{}))
}

func runWithRunner(arguments []string, input io.Reader, output, stderr io.Writer, sourceRunner planner.SourceRunner) int {
	if len(arguments) == 1 && arguments[0] == "worker" {
		if err := planner.RunWorker(context.Background(), input, output); err != nil {
			fmt.Fprintln(stderr, "runner request failed")
			return 1
		}
		return 0
	}
	if len(arguments) == 0 || arguments[0] != "execute" {
		writeUsage(stderr)
		return 2
	}
	flags := flag.NewFlagSet("thread-keep-runner execute", flag.ContinueOnError)
	flags.SetOutput(stderr)
	requestPath := flags.String("request-file", "", "absolute non-secret request file")
	credentialPath := flags.String("credential-file", "", "absolute checkout credential file")
	resultPath := flags.String("result-file", "", "absolute result file")
	credentialWait := flags.Duration("credential-wait-timeout", 30*time.Second, "credential file wait timeout")
	executionTimeout := flags.Duration("execution-timeout", 0, "source execution timeout")
	if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 || *executionTimeout < 0 {
		writeUsage(stderr)
		return 2
	}
	options := protocol.FileExecutionOptions{RequestPath: *requestPath, CredentialPath: *credentialPath, ResultPath: *resultPath, CredentialWaitTimeout: *credentialWait}
	executionCtx := context.Background()
	cancel := func() {}
	if *executionTimeout > 0 {
		executionCtx, cancel = context.WithTimeout(executionCtx, *executionTimeout)
	}
	defer cancel()
	if err := protocol.ExecuteFiles(executionCtx, options, sourceRunner); err != nil {
		fmt.Fprintln(stderr, "runner file execution failed")
		return 1
	}
	return 0
}

func writeUsage(stderr io.Writer) {
	fmt.Fprintln(stderr, "usage: thread-keep-runner worker | thread-keep-runner execute --request-file PATH --credential-file PATH --result-file PATH [--credential-wait-timeout DURATION] [--execution-timeout DURATION]")
}
