package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRunSignsManifestWithoutLeakingPrivateKey(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	payload := []byte(`{"schema_version":1,"packs":[]}`)
	directory := t.TempDir()
	payloadPath := filepath.Join(directory, "manifest.json")
	keyPath := filepath.Join(directory, "private-key")
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		t.Fatalf("WriteFile(payload): %v", err)
	}
	encodedPrivateKey := base64.StdEncoding.EncodeToString(privateKey)
	if err := os.WriteFile(keyPath, []byte(encodedPrivateKey), 0o600); err != nil {
		t.Fatalf("WriteFile(private key): %v", err)
	}
	var output bytes.Buffer
	if err := run([]string{"--payload", payloadPath, "--private-key", keyPath}, &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if bytes.Contains(output.Bytes(), privateKey) || bytes.Contains(output.Bytes(), []byte(encodedPrivateKey)) {
		t.Fatal("signer output contains private key material")
	}
	var envelope manifestEnvelope
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	signedPayload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if !bytes.Equal(signedPayload, payload) || !ed25519.Verify(publicKey, signedPayload, signature) {
		t.Fatal("signer output does not verify against the manifest payload")
	}
}

func TestRunRejectsInvalidArgumentsAndPrivateKey(t *testing.T) {
	if err := run(nil, &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want usage error")
	}
	directory := t.TempDir()
	payloadPath := filepath.Join(directory, "manifest.json")
	keyPath := filepath.Join(directory, "private-key")
	if err := os.WriteFile(payloadPath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("WriteFile(payload): %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not-a-key"), 0o600); err != nil {
		t.Fatalf("WriteFile(private key): %v", err)
	}
	if err := run([]string{"--payload", payloadPath, "--private-key", keyPath}, &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want private key validation error")
	}
}
