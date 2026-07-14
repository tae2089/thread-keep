package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

type Transport interface {
	ReadObject(ctx context.Context, id string) ([]byte, error)
	PublishObject(ctx context.Context, id string, contents []byte) (bool, error)
	ReadRef(ctx context.Context, refName string) (Ref, error)
	CompareAndSwapRef(ctx context.Context, refName string, expected, next Ref) (Ref, error)
}

type AddressKind string

const (
	AddressFilesystem AddressKind = "filesystem"
	AddressHTTP       AddressKind = "http"
)

var _ Transport = (*FileSystem)(nil)

func Dial(address string) (Transport, error) {
	normalized, kind, err := NormalizeAddress(address)
	if err != nil {
		return nil, err
	}
	if kind == AddressHTTP {
		return NewHTTP(normalized, os.Getenv(TokenEnvironmentVariable)), nil
	}
	return Open(normalized)
}

func NormalizeAddress(value string) (string, AddressKind, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", domain.NewError(domain.CodeValidation, errors.New("remote address must not be empty"))
	}
	if strings.Contains(value, "://") {
		normalized, err := normalizeURLAddress(value)
		if err != nil {
			return "", "", err
		}
		return normalized, AddressHTTP, nil
	}
	resolved, err := normalizeFilesystemAddress(value)
	if err != nil {
		return "", "", err
	}
	return resolved, AddressFilesystem, nil
}

func normalizeURLAddress(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", domain.NewError(domain.CodeValidation, fmt.Errorf("parse remote URL: %w", err))
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", domain.NewError(domain.CodeValidation, fmt.Errorf("remote URL scheme %q is unsupported", parsed.Scheme))
	}
	if parsed.User != nil {
		return "", domain.NewError(domain.CodeValidation, errors.New("remote URL must not embed credentials"))
	}
	if parsed.RawQuery != "" {
		return "", domain.NewError(domain.CodeValidation, errors.New("remote URL must not contain a query"))
	}
	if parsed.Fragment != "" {
		return "", domain.NewError(domain.CodeValidation, errors.New("remote URL must not contain a fragment"))
	}
	if parsed.Host == "" {
		return "", domain.NewError(domain.CodeValidation, errors.New("remote URL must include a host"))
	}
	if scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		return "", domain.NewError(domain.CodeValidation, errors.New("plain http remote is allowed only for loopback hosts"))
	}
	return scheme + "://" + parsed.Host + strings.TrimRight(parsed.EscapedPath(), "/"), nil
}

func normalizeFilesystemAddress(value string) (string, error) {
	if !filepath.IsAbs(value) {
		return "", domain.NewError(domain.CodeValidation, errors.New("remote path must be absolute"))
	}
	resolved, err := filepath.EvalSymlinks(value)
	if err != nil {
		return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("resolve remote path: %w", err))
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("inspect remote path: %w", err))
	}
	if !info.IsDir() {
		return "", domain.NewError(domain.CodeValidation, errors.New("remote path must be a directory"))
	}
	return resolved, nil
}

func ValidateObjectBytes(id string, contents []byte) error {
	return validateObjectBytes(id, contents)
}

func isLoopbackHost(hostname string) bool {
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}
