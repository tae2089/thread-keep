package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

type AppTokenConfig struct {
	APIBaseURL     string
	AppID          string
	InstallationID int64
	PrivateKeyPEM  []byte
}

type AppTokenSource struct {
	apiBaseURL     string
	appID          string
	installationID int64
	privateKey     *rsa.PrivateKey
	client         *http.Client
	now            func() time.Time
}

type installationTokenRequest struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func NewAppTokenSource(config AppTokenConfig) (*AppTokenSource, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.APIBaseURL), "/")
	if baseURL == "" || strings.TrimSpace(config.AppID) == "" || config.InstallationID < 1 || len(config.PrivateKeyPEM) == 0 {
		return nil, domain.NewError(domain.CodeValidation, errors.New("github app token configuration is incomplete"))
	}
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("github app token API URL is invalid"))
	}
	key, err := parseRSAPrivateKey(config.PrivateKeyPEM)
	if err != nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("github app private key is invalid"))
	}
	return &AppTokenSource{apiBaseURL: baseURL, appID: strings.TrimSpace(config.AppID), installationID: config.InstallationID, privateKey: key, client: newHTTPClient(), now: time.Now}, nil
}

func (s *AppTokenSource) Token(ctx context.Context, repository string, permissions map[string]string) (InstallationToken, error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || len(permissions) == 0 {
		return InstallationToken{}, domain.NewError(domain.CodeValidation, errors.New("github installation token scope is invalid"))
	}
	for permission, access := range permissions {
		if permission == "" || (access != "read" && access != "write") {
			return InstallationToken{}, domain.NewError(domain.CodeValidation, errors.New("github installation permission is invalid"))
		}
	}
	jwt, err := s.signJWT()
	if err != nil {
		return InstallationToken{}, domain.NewError(domain.CodeLocalStorage, errors.New("sign github app authentication"))
	}
	target := s.apiBaseURL + "/app/installations/" + strconv.FormatInt(s.installationID, 10) + "/access_tokens"
	requestBody := installationTokenRequest{Repositories: []string{parts[1]}, Permissions: permissions}
	var response installationTokenResponse
	adapter := Adapter{client: s.client}
	if err := adapter.callJSON(ctx, http.MethodPost, target, jwt, requestBody, &response, http.StatusCreated, http.StatusOK); err != nil {
		return InstallationToken{}, err
	}
	if response.Token == "" || response.ExpiresAt.Before(s.now().Add(time.Minute)) {
		return InstallationToken{}, domain.NewError(domain.CodeAuth, errors.New("github installation token response is invalid or expiring"))
	}
	return InstallationToken{Value: response.Token, ExpiresAt: response.ExpiresAt.UTC()}, nil
}

func (s *AppTokenSource) signJWT() (string, error) {
	now := s.now().UTC()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": s.appID})
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, s.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func parseRSAPrivateKey(contents []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(contents)
	if block == nil {
		return nil, errors.New("missing PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return key, nil
}
