package github

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/forge"
)

type staticTokenSource struct {
	token     string
	expiresAt time.Time
}

func TestDecodeWebhookVerifiesSignatureBeforeDecoding(t *testing.T) {
	const secret = "webhook-fixture-secret"
	adapter := newTestAdapter(t, nil, secret)
	headers := http.Header{"X-Github-Event": {"pull_request"}, "X-Github-Delivery": {"delivery-1"}, "X-Hub-Signature-256": {"sha256=" + strings.Repeat("0", 64)}}
	if _, err := adapter.DecodeWebhook(headers, []byte(`not-json`)); domain.CodeOf(err) != domain.CodeAuth {
		t.Fatalf("DecodeWebhook(invalid signature) error = %v, want auth", err)
	}
	body := []byte(`{"action":"synchronize","number":42,"repository":{"full_name":"owner/repository"},"pull_request":{"base":{"ref":"main","sha":"` + strings.Repeat("a", 40) + `"},"head":{"ref":"feature","sha":"` + strings.Repeat("b", 40) + `"}},"installation":{"id":7}}`)
	headers.Set("X-Hub-Signature-256", signWebhook(secret, body))
	event, err := adapter.DecodeWebhook(headers, body)
	if err != nil {
		t.Fatalf("DecodeWebhook(valid) error = %v", err)
	}
	if event.DeliveryID != "delivery-1" || event.Change.Repository != "owner/repository" || event.Change.Number != 42 || event.Action != forge.ForgeActionSynchronize {
		t.Fatalf("DecodeWebhook(valid) = %+v", event)
	}
}

func TestGetChangeRefetchesAuthoritativePullRequest(t *testing.T) {
	const token = "installation-token-fixture"
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/repos/owner/repository/pulls/42" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("missing installation authorization")
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"number": 42, "state": "closed", "merged": true, "merge_commit_sha": strings.Repeat("c", 40), "base": map[string]any{"ref": "main", "sha": strings.Repeat("a", 40)}, "head": map[string]any{"ref": "feature", "sha": strings.Repeat("b", 40)}})
	}))
	t.Cleanup(api.Close)
	adapter := newTestAdapter(t, staticTokenSource{token: token, expiresAt: time.Now().Add(time.Hour)}, "secret")
	adapter.apiBaseURL = api.URL
	change, err := adapter.GetChange(context.Background(), domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42})
	if err != nil {
		t.Fatalf("GetChange() error = %v", err)
	}
	if !change.Merged || change.MergeSHA != strings.Repeat("c", 40) || change.State != forge.ChangeMerged || change.BaseRef != "main" {
		t.Fatalf("GetChange() = %+v", change)
	}
}

func TestUpsertCheckMapsInformationalConclusions(t *testing.T) {
	const token = "check-token-fixture"
	requests := make(chan map[string]any, 2)
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/repos/owner/repository/check-runs" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatalf("decode check request: %v", err)
		}
		requests <- payload
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(`{"id":123}`))
	}))
	t.Cleanup(api.Close)
	adapter := newTestAdapter(t, staticTokenSource{token: token, expiresAt: time.Now().Add(time.Hour)}, "secret")
	adapter.apiBaseURL = api.URL
	key := domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}
	for _, state := range []forge.CheckState{forge.CheckReady, forge.CheckReviewRequired} {
		if err := adapter.UpsertCheck(context.Background(), forge.CheckInput{Change: key, HeadSHA: strings.Repeat("b", 40), State: state, Summary: "bounded summary", PlanURL: "https://example.invalid/plans/1"}); err != nil {
			t.Fatalf("UpsertCheck(%s) error = %v", state, err)
		}
	}
	ready := <-requests
	review := <-requests
	if ready["status"] != "completed" || ready["conclusion"] != "success" || review["conclusion"] != "neutral" {
		t.Fatalf("check payloads = ready:%+v review:%+v", ready, review)
	}
}

func TestReconcileCheckUsesCanonicalRunAndSupersedesDuplicates(t *testing.T) {
	const token = "check-token-fixture"
	headSHA := strings.Repeat("b", 40)
	externalID := "github:owner/repository#42@" + headSHA
	patched := make(map[string]map[string]any)
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repository/commits/"+headSHA+"/check-runs":
			_ = json.NewEncoder(writer).Encode(map[string]any{"total_count": 3, "check_runs": []map[string]any{
				{"id": 9, "name": checkName, "head_sha": headSHA, "external_id": externalID},
				{"id": 3, "name": checkName, "head_sha": headSHA, "external_id": externalID},
				{"id": 11, "name": "other", "head_sha": headSHA, "external_id": externalID},
			}})
		case request.Method == http.MethodPatch && strings.HasPrefix(request.URL.Path, "/repos/owner/repository/check-runs/"):
			var payload map[string]any
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatalf("decode patch: %v", err)
			}
			patched[strings.TrimPrefix(request.URL.Path, "/repos/owner/repository/check-runs/")] = payload
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(`{"id":3}`))
		default:
			t.Fatalf("unexpected request = %s %s", request.Method, request.URL.String())
		}
	}))
	t.Cleanup(api.Close)
	adapter := newTestAdapter(t, staticTokenSource{token: token, expiresAt: time.Now().Add(time.Hour)}, "secret")
	adapter.apiBaseURL = api.URL
	publication, err := adapter.ReconcileCheck(context.Background(), forge.CheckInput{Change: domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}, HeadSHA: headSHA, State: forge.CheckReady, Summary: "Plan is ready.", PlanURL: "https://example.invalid/plans/1", CheckRunID: 77})
	if err != nil || publication.CheckRunID != 3 {
		t.Fatalf("ReconcileCheck() = %+v, %v, want canonical 3", publication, err)
	}
	if patched["3"]["conclusion"] != "success" || patched["9"]["conclusion"] != "cancelled" {
		t.Fatalf("patched checks = %+v", patched)
	}
}

func TestAppTokenSourceScopesCheckoutGrantAndDoesNotLeakSecrets(t *testing.T) {
	privatePEM := generatePrivateKeyPEM(t)
	const minted = "minted-installation-token-fixture"
	var tokenRequest map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/app/installations/7/access_tokens" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer eyJ") {
			t.Fatal("installation token request is not authenticated with an app JWT")
		}
		if err := json.NewDecoder(request.Body).Decode(&tokenRequest); err != nil {
			t.Fatalf("decode token request: %v", err)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"token": minted, "expires_at": time.Now().Add(time.Hour).UTC()})
	}))
	t.Cleanup(api.Close)
	source, err := NewAppTokenSource(AppTokenConfig{APIBaseURL: api.URL, AppID: "123", InstallationID: 7, PrivateKeyPEM: privatePEM})
	if err != nil {
		t.Fatalf("NewAppTokenSource() error = %v", err)
	}
	adapter := newTestAdapter(t, source, "secret")
	adapter.apiBaseURL = api.URL
	grant, err := adapter.CheckoutGrant(context.Background(), forge.CheckoutGrantInput{Change: domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}})
	if err != nil {
		t.Fatalf("CheckoutGrant() error = %v", err)
	}
	if grant.Token != minted || grant.Repository != "owner/repository" || !grant.ExpiresAt.After(time.Now()) {
		t.Fatalf("CheckoutGrant() = %+v", grant)
	}
	permissions := tokenRequest["permissions"].(map[string]any)
	if permissions["contents"] != "read" || len(permissions) != 1 {
		t.Fatalf("token permissions = %+v, want contents:read only", permissions)
	}
	repositories := tokenRequest["repositories"].([]any)
	if len(repositories) != 1 || repositories[0] != "repository" {
		t.Fatalf("token repositories = %+v", repositories)
	}
	failedAPI := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, minted+" private material", http.StatusInternalServerError)
	}))
	t.Cleanup(failedAPI.Close)
	source.apiBaseURL = failedAPI.URL
	_, err = source.Token(context.Background(), "owner/repository", map[string]string{"contents": "read"})
	if err == nil || strings.Contains(err.Error(), minted) || strings.Contains(err.Error(), string(privatePEM)) {
		t.Fatalf("Token(failure) secret-safe error = %v", err)
	}
}

func TestAdapterRefusesRedirectWithoutForwardingToken(t *testing.T) {
	const token = "redirect-token-fixture"
	secondHopCalls := 0
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { secondHopCalls++ }))
	t.Cleanup(second.Close)
	first := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, second.URL, http.StatusFound)
	}))
	t.Cleanup(first.Close)
	adapter := newTestAdapter(t, staticTokenSource{token: token, expiresAt: time.Now().Add(time.Hour)}, "secret")
	adapter.apiBaseURL = first.URL
	_, err := adapter.GetChange(context.Background(), domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42})
	if domain.CodeOf(err) != domain.CodeLocalStorage || strings.Contains(err.Error(), token) || secondHopCalls != 0 {
		t.Fatalf("GetChange(redirect) = %v, second hop calls %d", err, secondHopCalls)
	}
}

func TestAdapterDistinguishesProviderDenialFromOutage(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		wantCode domain.ErrorCode
	}{
		{name: "denial", status: http.StatusForbidden, wantCode: domain.CodeAuth},
		{name: "outage", status: http.StatusServiceUnavailable, wantCode: domain.CodeLocalStorage},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				http.Error(writer, "sensitive provider fixture", test.status)
			}))
			t.Cleanup(api.Close)
			adapter := newTestAdapter(t, staticTokenSource{token: "provider-token-fixture", expiresAt: time.Now().Add(time.Hour)}, "secret")
			adapter.apiBaseURL = api.URL
			_, err := adapter.GetChange(context.Background(), domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42})
			if domain.CodeOf(err) != test.wantCode || strings.Contains(err.Error(), "sensitive provider fixture") || strings.Contains(err.Error(), "provider-token-fixture") {
				t.Fatalf("GetChange() error = %v, want code %s without provider body or token", err, test.wantCode)
			}
		})
	}
}

func TestAdapterHTTPClientHasBoundedTransportAndNoWholeRequestTimeout(t *testing.T) {
	adapter := newTestAdapter(t, staticTokenSource{token: "token", expiresAt: time.Now().Add(time.Hour)}, "secret")
	transport, ok := adapter.client.Transport.(*http.Transport)
	if !ok || transport.DialContext == nil {
		t.Fatalf("adapter transport = %T", adapter.client.Transport)
	}
	if transport.ResponseHeaderTimeout != githubHeaderTimeout || githubDialTimeout != 10*time.Second || adapter.client.Timeout != 0 {
		t.Fatalf("adapter timeouts = header %s, dial %s, whole request %s", transport.ResponseHeaderTimeout, githubDialTimeout, adapter.client.Timeout)
	}
	if adapter.client.CheckRedirect == nil {
		t.Fatal("adapter redirect policy is nil")
	}
}

func (s staticTokenSource) Token(context.Context, string, map[string]string) (InstallationToken, error) {
	return InstallationToken{Value: s.token, ExpiresAt: s.expiresAt}, nil
}

func newTestAdapter(t *testing.T, source TokenSource, secret string) *Adapter {
	t.Helper()
	adapter, err := NewAdapter(AdapterConfig{APIBaseURL: "https://api.github.invalid", WebhookSecret: []byte(secret), TokenSource: source})
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	return adapter
}

func signWebhook(secret string, body []byte) string {
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write(body)
	return "sha256=" + hex.EncodeToString(digest.Sum(nil))
}

func generatePrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
