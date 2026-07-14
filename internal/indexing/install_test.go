package indexing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
)

func TestInstallerPublishesVerifiedCurrentPlatformPack(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("#!/bin/sh\nprintf 'typescript pack'\n")
	digest := sha256Hex(artifact)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			manifest := testManifest(server.URL+"/assets/typescript", runtime.GOOS, runtime.GOARCH, int64(len(artifact)), digest)
			writeSignedManifest(t, writer, manifest, privateKey)
		case "/assets/typescript":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := testUserConfigDir(t)
	installer := testInstaller(server, publicKey, configDir)

	if err := installer.Install(context.Background(), []Language{TypeScript}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	activation, err := loadPackActivation(configDir, TypeScript)
	if err != nil {
		t.Fatalf("loadPackActivation() error = %v", err)
	}
	if activation.PackID != packID(TypeScript) || activation.Version != "1.0.0" || activation.ProtocolVersion != protocolVersion || activation.Size != int64(len(artifact)) || activation.SHA256 != digest {
		t.Fatalf("installed activation = %#v", activation)
	}
	target := filepath.Join(configDir, "thread-keep", "packs", "objects", packID(TypeScript), digest)
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(installed pack): %v", err)
	}
	if string(contents) != string(artifact) {
		t.Fatalf("installed pack = %q, want %q", contents, artifact)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(installed pack): %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Fatalf("installed pack mode = %v, want executable regular file", info.Mode())
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.ts"), []byte("export function run() {}"), 0o644); err != nil {
		t.Fatalf("WriteFile(app.ts): %v", err)
	}
	statuses, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if statuses[1].State != domain.IndexerInstalled || statuses[1].Path != target || statuses[1].Version != "1.0.0" || statuses[1].SHA256 != digest {
		t.Fatalf("TypeScript status = %#v", statuses[1])
	}
}

func TestInstallerUsesWindowsExecutableNameForManagedPack(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("windows pack")
	digest := sha256Hex(artifact)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			writeSignedManifest(t, writer, testManifest(server.URL+"/assets/typescript", "windows", runtime.GOARCH, int64(len(artifact)), digest), privateKey)
		case "/assets/typescript":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := t.TempDir()
	installer := testInstaller(server, publicKey, configDir)
	installer.GOOS = "windows"

	if err := installer.Install(context.Background(), []Language{TypeScript}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	target := filepath.Join(packObjectDirectory(configDir, TypeScript), digest+".exe")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("Stat(installed Windows pack): %v", err)
	}
}

func TestInstallerPublishesVerifiedCurrentPlatformJavaScriptPack(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("#!/bin/sh\nprintf 'javascript pack'\n")
	digest := sha256Hex(artifact)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			manifest := testManifest(server.URL+"/assets/javascript", runtime.GOOS, runtime.GOARCH, int64(len(artifact)), digest)
			manifest.Packs[0].ID = packID(JavaScript)
			writeSignedManifest(t, writer, manifest, privateKey)
		case "/assets/javascript":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := t.TempDir()
	installer := testInstaller(server, publicKey, configDir)

	if err := installer.Install(context.Background(), []Language{JavaScript}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	target := packObjectPath(configDir, JavaScript, digest)
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(installed JavaScript pack): %v", err)
	}
	if string(contents) != string(artifact) {
		t.Fatalf("installed JavaScript pack = %q, want %q", contents, artifact)
	}
}

func TestInstallerPublishesVerifiedCurrentPlatformPythonPack(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("#!/bin/sh\nprintf 'python pack'\n")
	digest := sha256Hex(artifact)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			manifest := testManifest(server.URL+"/assets/python", runtime.GOOS, runtime.GOARCH, int64(len(artifact)), digest)
			manifest.Packs[0].ID = packID(Python)
			writeSignedManifest(t, writer, manifest, privateKey)
		case "/assets/python":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := t.TempDir()
	installer := testInstaller(server, publicKey, configDir)

	if err := installer.Install(context.Background(), []Language{Python}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	target := packObjectPath(configDir, Python, digest)
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(installed Python pack): %v", err)
	}
	if string(contents) != string(artifact) {
		t.Fatalf("installed Python pack = %q, want %q", contents, artifact)
	}
}

func TestInstallerPublishesVerifiedCurrentPlatformJavaPack(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("#!/bin/sh\nprintf 'java pack'\n")
	digest := sha256Hex(artifact)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			manifest := testManifest(server.URL+"/assets/java", runtime.GOOS, runtime.GOARCH, int64(len(artifact)), digest)
			manifest.Packs[0].ID = packID(Java)
			writeSignedManifest(t, writer, manifest, privateKey)
		case "/assets/java":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := t.TempDir()
	installer := testInstaller(server, publicKey, configDir)

	if err := installer.Install(context.Background(), []Language{Java}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	target := packObjectPath(configDir, Java, digest)
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(installed Java pack): %v", err)
	}
	if string(contents) != string(artifact) {
		t.Fatalf("installed Java pack = %q, want %q", contents, artifact)
	}
}

func TestInstallerPublishesVerifiedCurrentPlatformKotlinPack(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("#!/bin/sh\nprintf 'kotlin pack'\n")
	digest := sha256Hex(artifact)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			manifest := testManifest(server.URL+"/assets/kotlin", runtime.GOOS, runtime.GOARCH, int64(len(artifact)), digest)
			manifest.Packs[0].ID = packID(Kotlin)
			writeSignedManifest(t, writer, manifest, privateKey)
		case "/assets/kotlin":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := t.TempDir()
	installer := testInstaller(server, publicKey, configDir)

	if err := installer.Install(context.Background(), []Language{Kotlin}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	target := packObjectPath(configDir, Kotlin, digest)
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(installed Kotlin pack): %v", err)
	}
	if string(contents) != string(artifact) {
		t.Fatalf("installed Kotlin pack = %q, want %q", contents, artifact)
	}
}

func TestInstallerPublishesVerifiedCurrentPlatformRustPack(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("#!/bin/sh\nprintf 'rust pack'\n")
	digest := sha256Hex(artifact)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			manifest := testManifest(server.URL+"/assets/rust", runtime.GOOS, runtime.GOARCH, int64(len(artifact)), digest)
			manifest.Packs[0].ID = packID(Rust)
			writeSignedManifest(t, writer, manifest, privateKey)
		case "/assets/rust":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := t.TempDir()
	installer := testInstaller(server, publicKey, configDir)

	if err := installer.Install(context.Background(), []Language{Rust}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	target := packObjectPath(configDir, Rust, digest)
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(installed Rust pack): %v", err)
	}
	if string(contents) != string(artifact) {
		t.Fatalf("installed Rust pack = %q, want %q", contents, artifact)
	}
}

func TestInstallerSyncActivatesVerifiedReplacement(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("first")
	version := "1.0.0"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			value := testManifest(server.URL+"/assets/typescript", runtime.GOOS, runtime.GOARCH, int64(len(artifact)), sha256Hex(artifact))
			value.Packs[0].Version = version
			writeSignedManifest(t, writer, value, privateKey)
		case "/assets/typescript":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := t.TempDir()
	installer := testInstaller(server, publicKey, configDir)
	if err := installer.Install(context.Background(), []Language{TypeScript}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	firstDigest := sha256Hex(artifact)

	artifact = []byte("second")
	version = "2.0.0"
	if err := installer.Sync(context.Background(), []Language{TypeScript}); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	activation, err := loadPackActivation(configDir, TypeScript)
	if err != nil {
		t.Fatalf("loadPackActivation() error = %v", err)
	}
	secondDigest := sha256Hex(artifact)
	if activation.Version != version || activation.SHA256 != secondDigest {
		t.Fatalf("activation after sync = %#v", activation)
	}
	if _, err := os.Stat(packObjectPath(configDir, TypeScript, firstDigest)); err != nil {
		t.Fatalf("previous immutable artifact after sync: %v", err)
	}
	if contents, err := os.ReadFile(packObjectPath(configDir, TypeScript, secondDigest)); err != nil || string(contents) != string(artifact) {
		t.Fatalf("active artifact after sync = %q, %v", contents, err)
	}
}

func TestInstallerFailedSyncPreservesActivePack(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("first")
	valid := true
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			digest := sha256Hex(artifact)
			if !valid {
				digest = sha256Hex([]byte("different"))
			}
			value := testManifest(server.URL+"/assets/typescript", runtime.GOOS, runtime.GOARCH, int64(len(artifact)), digest)
			writeSignedManifest(t, writer, value, privateKey)
		case "/assets/typescript":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := t.TempDir()
	installer := testInstaller(server, publicKey, configDir)
	if err := installer.Install(context.Background(), []Language{TypeScript}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	before, err := loadPackActivation(configDir, TypeScript)
	if err != nil {
		t.Fatalf("loadPackActivation() before sync error = %v", err)
	}
	valid = false
	if err := installer.Sync(context.Background(), []Language{TypeScript}); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Sync() error = %v, want validation", err)
	}
	after, err := loadPackActivation(configDir, TypeScript)
	if err != nil {
		t.Fatalf("loadPackActivation() after sync error = %v", err)
	}
	if after != before {
		t.Fatalf("activation after failed sync = %#v, want %#v", after, before)
	}
}

func TestInstallerRejectsManifestVersionMismatch(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("pack")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			writeSignedManifest(t, writer, testManifest(server.URL+"/assets/typescript", runtime.GOOS, runtime.GOARCH, int64(len(artifact)), sha256Hex(artifact)), privateKey)
		case "/assets/typescript":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := t.TempDir()
	installer := testInstaller(server, publicKey, configDir)
	installer.ExpectedVersion = "2.0.0"
	if err := installer.Sync(context.Background(), []Language{TypeScript}); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Sync() error = %v, want validation", err)
	}
	if _, err := os.Stat(packDirectory(configDir)); !os.IsNotExist(err) {
		t.Fatalf("version mismatch created pack storage: %v", err)
	}
}

func TestOfficialManifestURLSelectsLatestOrExactStableVersion(t *testing.T) {
	if got, err := officialManifestURLForVersion(""); err != nil || got != officialManifestURL {
		t.Fatalf("latest manifest URL = %q, %v", got, err)
	}
	if got, err := officialManifestURLForVersion("1.2.3"); err != nil || got != "https://github.com/tae2089/thread-keep/releases/download/v1.2.3/thread-keep-indexers-manifest-v1.json" {
		t.Fatalf("exact manifest URL = %q, %v", got, err)
	}
	for _, version := range []string{"v1.2.3", "1.2", "1.02.3", "1.2.3-beta"} {
		if _, err := officialManifestURLForVersion(version); domain.CodeOf(err) != domain.CodeValidation {
			t.Fatalf("officialManifestURLForVersion(%q) error = %v, want validation", version, err)
		}
	}
}

func TestSyncDetectedRejectsInvalidVersionBeforeNoop(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go): %v", err)
	}
	if _, err := SyncDetected(context.Background(), root, "v1.2.3"); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("SyncDetected() error = %v, want validation", err)
	}
}

func TestInstallerRejectsInvalidManifestOrArtifactWithoutReplacingExistingPack(t *testing.T) {
	tests := []struct {
		name     string
		manifest func(string) manifest
		write    func(http.ResponseWriter)
		wantCode domain.ErrorCode
	}{
		{
			name: "invalid signature",
			manifest: func(assetURL string) manifest {
				return testManifest(assetURL, runtime.GOOS, runtime.GOARCH, 4, sha256Hex([]byte("next")))
			},
			write:    func(writer http.ResponseWriter) { _, _ = writer.Write([]byte("next")) },
			wantCode: domain.CodeValidation,
		},
		{
			name: "digest mismatch",
			manifest: func(assetURL string) manifest {
				return testManifest(assetURL, runtime.GOOS, runtime.GOARCH, 4, sha256Hex([]byte("next")))
			},
			write:    func(writer http.ResponseWriter) { _, _ = writer.Write([]byte("evil")) },
			wantCode: domain.CodeValidation,
		},
		{
			name: "unsupported platform",
			manifest: func(assetURL string) manifest {
				return testManifest(assetURL, "unsupported", "unsupported", 4, sha256Hex([]byte("next")))
			},
			write:    func(writer http.ResponseWriter) { _, _ = writer.Write([]byte("next")) },
			wantCode: domain.CodeValidation,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			publicKey, privateKey := testSigningKey(t)
			_, wrongPrivateKey := testSigningKey(t)
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/manifest":
					manifest := test.manifest(server.URL + "/assets/typescript")
					if test.name == "invalid signature" {
						writeSignedManifest(t, writer, manifest, wrongPrivateKey)
						return
					}
					writeSignedManifest(t, writer, manifest, privateKey)
				case "/assets/typescript":
					test.write(writer)
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()
			configDir := t.TempDir()
			target := filepath.Join(configDir, "thread-keep", "packs", packID(TypeScript))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("MkdirAll(target): %v", err)
			}
			if err := os.WriteFile(target, []byte("previous"), 0o644); err != nil {
				t.Fatalf("WriteFile(previous pack): %v", err)
			}
			installer := testInstaller(server, publicKey, configDir)
			if err := installer.Install(context.Background(), []Language{TypeScript}); domain.CodeOf(err) != test.wantCode {
				t.Fatalf("Install() error = %v, want %s", err, test.wantCode)
			}
			contents, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("ReadFile(previous pack): %v", err)
			}
			if string(contents) != "previous" {
				t.Fatalf("pack after failed install = %q, want previous contents", contents)
			}
		})
	}
}

func TestInstallerRejectsInvalidVerifiedManifestContent(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	installer := Installer{
		PublicKey:             publicKey,
		TrustedArtifactPrefix: "https://github.com/tae2089/thread-keep/releases/download/",
	}
	tests := []struct {
		name   string
		mutate func(*manifest)
	}{
		{
			name: "unsupported schema",
			mutate: func(value *manifest) {
				value.SchemaVersion++
			},
		},
		{
			name: "invalid pack ID",
			mutate: func(value *manifest) {
				value.Packs[0].ID = "unexpected"
			},
		},
		{
			name: "missing pack version",
			mutate: func(value *manifest) {
				value.Packs[0].Version = ""
			},
		},
		{
			name: "non-stable pack version",
			mutate: func(value *manifest) {
				value.Packs[0].Version = "1.2.3-beta"
			},
		},
		{
			name: "unsupported protocol",
			mutate: func(value *manifest) {
				value.Packs[0].ProtocolVersion++
			},
		},
		{
			name: "untrusted asset origin",
			mutate: func(value *manifest) {
				value.Packs[0].Assets[0].URL = "https://example.invalid/pack"
			},
		},
		{
			name: "invalid asset size",
			mutate: func(value *manifest) {
				value.Packs[0].Assets[0].Size = 0
			},
		},
		{
			name: "invalid digest",
			mutate: func(value *manifest) {
				value.Packs[0].Assets[0].SHA256 = "invalid"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := testManifest("https://github.com/tae2089/thread-keep/releases/download/v1/thread-keep-index-typescript", runtime.GOOS, runtime.GOARCH, 4, sha256Hex([]byte("next")))
			test.mutate(&value)
			recorder := httptest.NewRecorder()
			writeSignedManifest(t, recorder, value, privateKey)
			if _, err := installer.verifyManifest(recorder.Body.Bytes()); domain.CodeOf(err) != domain.CodeValidation {
				t.Fatalf("verifyManifest() error = %v, want validation", err)
			}
		})
	}
}

func TestInstallerRejectsUntrustedArtifactRedirect(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			manifest := testManifest(server.URL+"/assets/typescript", runtime.GOOS, runtime.GOARCH, 4, sha256Hex([]byte("next")))
			writeSignedManifest(t, writer, manifest, privateKey)
		case "/assets/typescript":
			http.Redirect(writer, request, "http://example.invalid/pack", http.StatusFound)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	installer := testInstaller(server, publicKey, t.TempDir())
	if err := installer.Install(context.Background(), []Language{TypeScript}); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Install() error = %v, want validation", err)
	}
}

func TestInstallerCleansTemporaryArtifactWhenImmutablePublishTargetIsInvalid(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("next")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			manifest := testManifest(server.URL+"/assets/typescript", runtime.GOOS, runtime.GOARCH, int64(len(artifact)), sha256Hex(artifact))
			writeSignedManifest(t, writer, manifest, privateKey)
		case "/assets/typescript":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := t.TempDir()
	directory := packObjectDirectory(configDir, TypeScript)
	target := packObjectPath(configDir, TypeScript, sha256Hex(artifact))
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("MkdirAll(target directory): %v", err)
	}
	installer := testInstaller(server, publicKey, configDir)
	if err := installer.Install(context.Background(), []Language{TypeScript}); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Install() error = %v, want validation", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(target): %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("target mode = %v, want original directory", info.Mode())
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("ReadDir(pack directory): %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != packExecutableName(sha256Hex(artifact)) {
		t.Fatalf("pack directory after failed publish = %#v, want only original target", entries)
	}
}

func TestInstallerDoesNotOverwriteExistingExecutablePack(t *testing.T) {
	publicKey, privateKey := testSigningKey(t)
	artifact := []byte("next")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/manifest":
			manifest := testManifest(server.URL+"/assets/typescript", runtime.GOOS, runtime.GOARCH, int64(len(artifact)), sha256Hex(artifact))
			writeSignedManifest(t, writer, manifest, privateKey)
		case "/assets/typescript":
			_, _ = writer.Write(artifact)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	configDir := t.TempDir()
	target := filepath.Join(configDir, "thread-keep", "packs", packID(TypeScript))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("MkdirAll(target): %v", err)
	}
	if err := os.WriteFile(target, []byte("existing"), 0o755); err != nil {
		t.Fatalf("WriteFile(existing pack): %v", err)
	}
	installer := testInstaller(server, publicKey, configDir)
	if err := installer.Install(context.Background(), []Language{TypeScript}); domain.CodeOf(err) != domain.CodeBusy {
		t.Fatalf("Install() error = %v, want busy", err)
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(existing pack): %v", err)
	}
	if string(contents) != "existing" {
		t.Fatalf("existing pack after install = %q, want preserved contents", contents)
	}
}

func TestInstallerRejectsRedirectToNonStandardHTTPSPort(t *testing.T) {
	installer := Installer{AllowedRedirectHosts: map[string]bool{"github.com": true}}
	redirect, err := url.Parse("https://github.com:444/release")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := installer.validateRedirectURL(redirect); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("validateRedirectURL() error = %v, want validation", err)
	}
}

func TestInstallDetectedSkipsManifestWithoutDetectedMissingPack(t *testing.T) {
	configDir := testUserConfigDir(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go): %v", err)
	}
	statuses, err := InstallDetected(context.Background(), root)
	if err != nil {
		t.Fatalf("InstallDetected() error = %v", err)
	}
	if len(statuses) != 7 || statuses[0].State != domain.IndexerBuiltin || statuses[1].State != domain.IndexerMissing || statuses[1].Detected || statuses[2].Language != string(JavaScript) || statuses[2].State != domain.IndexerMissing || statuses[2].Detected || statuses[3].Language != string(Python) || statuses[3].State != domain.IndexerMissing || statuses[3].Detected || statuses[4].Language != string(Java) || statuses[4].State != domain.IndexerMissing || statuses[4].Detected || statuses[5].Language != string(Kotlin) || statuses[5].State != domain.IndexerMissing || statuses[5].Detected || statuses[6].Language != string(Rust) || statuses[6].State != domain.IndexerMissing || statuses[6].Detected {
		t.Fatalf("InstallDetected() statuses = %#v", statuses)
	}
	if _, err := os.Stat(filepath.Join(configDir, "thread-keep")); !os.IsNotExist(err) {
		t.Fatalf("InstallDetected() created pack storage: %v", err)
	}
}

func TestNewOfficialInstallerRequiresReleasePublicKey(t *testing.T) {
	original := officialManifestPublicKeyBase64
	officialManifestPublicKeyBase64 = ""
	t.Cleanup(func() { officialManifestPublicKeyBase64 = original })
	if _, err := newOfficialInstaller(); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("newOfficialInstaller() error = %v, want validation", err)
	}
}

func testSigningKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	return publicKey, privateKey
}

func testInstaller(server *httptest.Server, publicKey ed25519.PublicKey, configDir string) Installer {
	return Installer{
		Client:                server.Client(),
		ManifestURL:           server.URL + "/manifest",
		PublicKey:             publicKey,
		GOOS:                  runtime.GOOS,
		GOARCH:                runtime.GOARCH,
		UserConfigDir:         func() (string, error) { return configDir, nil },
		TrustedArtifactPrefix: server.URL + "/assets/",
		AllowHTTP:             true,
	}
}

func testManifest(assetURL, goos, goarch string, size int64, digest string) manifest {
	return manifest{
		SchemaVersion: manifestSchemaVersion,
		Packs: []manifestPack{{
			ID:              packID(TypeScript),
			Version:         "1.0.0",
			ProtocolVersion: protocolVersion,
			Assets: []manifestAsset{{
				GOOS:   goos,
				GOARCH: goarch,
				URL:    assetURL,
				Size:   size,
				SHA256: digest,
			}},
		}},
	}
}

func writeSignedManifest(t *testing.T, writer http.ResponseWriter, value manifest, privateKey ed25519.PrivateKey) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	envelope := manifestEnvelope{Payload: base64.StdEncoding.EncodeToString(payload), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))}
	if err := json.NewEncoder(writer).Encode(envelope); err != nil {
		t.Fatalf("encode manifest envelope: %v", err)
	}
}

func sha256Hex(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}
