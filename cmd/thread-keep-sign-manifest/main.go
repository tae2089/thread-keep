package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxPayloadBytes = 512 << 10

type manifestEnvelope struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("thread-keep-sign-manifest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	payloadPath := flags.String("payload", "", "path to manifest payload JSON")
	privateKeyPath := flags.String("private-key", "", "path to base64 Ed25519 private key")
	if err := flags.Parse(arguments); err != nil {
		return errors.New("usage: thread-keep-sign-manifest --payload <manifest.json> --private-key <private-key-file>")
	}
	if flags.NArg() != 0 || strings.TrimSpace(*payloadPath) == "" || strings.TrimSpace(*privateKeyPath) == "" {
		return errors.New("usage: thread-keep-sign-manifest --payload <manifest.json> --private-key <private-key-file>")
	}
	payload, err := readBoundedFile(*payloadPath, maxPayloadBytes)
	if err != nil {
		return fmt.Errorf("read manifest payload: %w", err)
	}
	keyContents, err := readBoundedFile(*privateKeyPath, 4096)
	if err != nil {
		return fmt.Errorf("read manifest private key: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(keyContents)))
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return errors.New("manifest private key is not a valid base64 Ed25519 private key")
	}
	envelope := manifestEnvelope{
		Payload:   base64.StdEncoding.EncodeToString(payload),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(key), payload)),
	}
	return json.NewEncoder(output).Encode(envelope)
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return contents, nil
}
