package remote

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
)

func TestNormalizeAddressAcceptsHTTPSAndLoopbackHTTP(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"https host", "https://context.example.com", "https://context.example.com"},
		{"https port and base path", " https://context.example.com:8443/base/ ", "https://context.example.com:8443/base"},
		{"uppercase scheme", "HTTPS://context.example.com", "https://context.example.com"},
		{"http ipv4 loopback", "http://127.0.0.1:9000", "http://127.0.0.1:9000"},
		{"http localhost", "http://localhost:9000", "http://localhost:9000"},
		{"http ipv6 loopback", "http://[::1]:9000", "http://[::1]:9000"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, kind, err := NormalizeAddress(testCase.in)
			if err != nil || got != testCase.want || kind != AddressHTTP {
				t.Fatalf("NormalizeAddress(%q) = %q, %q, %v, want %q, %q, nil", testCase.in, got, kind, err, testCase.want, AddressHTTP)
			}
		})
	}
}

func TestNormalizeAddressRejectsUnsafeURLs(t *testing.T) {
	cases := []struct{ name, in, wantSubstring string }{
		{"plain http non-loopback", "http://context.example.com", "loopback"},
		{"plain http private ip", "http://10.0.0.5:9000", "loopback"},
		{"user info", "https://alice:secret@context.example.com", "credentials"},
		{"query", "https://context.example.com?x=1", "query"},
		{"fragment", "https://context.example.com#top", "fragment"},
		{"missing host", "https://", "host"},
		{"unsupported scheme", "ftp://context.example.com", "scheme"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := NormalizeAddress(testCase.in)
			if domain.CodeOf(err) != domain.CodeValidation {
				t.Fatalf("NormalizeAddress(%q) error = %v, want %q", testCase.in, err, domain.CodeValidation)
			}
			if !strings.Contains(err.Error(), testCase.wantSubstring) {
				t.Fatalf("NormalizeAddress(%q) error = %v, want substring %q", testCase.in, err, testCase.wantSubstring)
			}
		})
	}
}

func TestNormalizeAddressKeepsFilesystemRules(t *testing.T) {
	directory := t.TempDir()
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	got, kind, err := NormalizeAddress(directory)
	if err != nil || got != resolved || kind != AddressFilesystem {
		t.Fatalf("NormalizeAddress(%q) = %q, %q, %v, want %q, %q, nil", directory, got, kind, err, resolved, AddressFilesystem)
	}
	if _, _, err := NormalizeAddress("relative/path"); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("NormalizeAddress(relative) error = %v, want %q", err, domain.CodeValidation)
	}
	if _, _, err := NormalizeAddress(filepath.Join(directory, "missing")); domain.CodeOf(err) != domain.CodeLocalStorage {
		t.Fatalf("NormalizeAddress(missing) error = %v, want %q", err, domain.CodeLocalStorage)
	}
}

func TestDialSelectsTransportByAddress(t *testing.T) {
	directory := t.TempDir()
	fsTransport, err := Dial(directory)
	if err != nil {
		t.Fatalf("Dial(filesystem) error = %v", err)
	}
	if _, ok := fsTransport.(*FileSystem); !ok {
		t.Fatalf("Dial(filesystem) transport = %T, want *FileSystem", fsTransport)
	}
	httpTransport, err := Dial("https://context.example.com")
	if err != nil {
		t.Fatalf("Dial(https) error = %v", err)
	}
	if _, ok := httpTransport.(*HTTP); !ok {
		t.Fatalf("Dial(https) transport = %T, want *HTTP", httpTransport)
	}
	if _, err := Dial("ftp://context.example.com"); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Dial(ftp) error = %v, want %q", err, domain.CodeValidation)
	}
}
