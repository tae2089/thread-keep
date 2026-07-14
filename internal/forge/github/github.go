package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/forge"
)

const (
	apiVersion             = "2026-03-10"
	checkName              = "thread-keep/context-plan"
	maxWebhookBytes        = 1 << 20
	maxAPIResponseBytes    = 1 << 20
	maxCheckSummaryBytes   = 4096
	githubDialTimeout      = 10 * time.Second
	githubHeaderTimeout    = 30 * time.Second
	redirectRefusalMessage = "github redirects are not followed"
)

type InstallationToken struct {
	Value     string
	ExpiresAt time.Time
}

type TokenSource interface {
	Token(ctx context.Context, repository string, permissions map[string]string) (InstallationToken, error)
}

type AdapterConfig struct {
	APIBaseURL    string
	WebhookSecret []byte
	TokenSource   TokenSource
}

type Adapter struct {
	apiBaseURL    string
	webhookSecret []byte
	tokenSource   TokenSource
	client        *http.Client
}

type webhookPayload struct {
	Action     forge.ForgeAction `json:"action"`
	Number     int               `json:"number"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Base struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

type pullRequestResponse struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Base           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

type checkRunRequest struct {
	Name       string         `json:"name,omitempty"`
	HeadSHA    string         `json:"head_sha,omitempty"`
	Status     string         `json:"status"`
	Conclusion string         `json:"conclusion,omitempty"`
	DetailsURL string         `json:"details_url,omitempty"`
	ExternalID string         `json:"external_id,omitempty"`
	Output     checkRunOutput `json:"output"`
}

type checkRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

type checkRunResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	ExternalID string `json:"external_id"`
}

type checkRunsResponse struct {
	TotalCount int                `json:"total_count"`
	CheckRuns  []checkRunResponse `json:"check_runs"`
}

var _ forge.Forge = (*Adapter)(nil)
var _ forge.CheckPublisher = (*Adapter)(nil)

func NewAdapter(config AdapterConfig) (*Adapter, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.APIBaseURL), "/")
	if baseURL == "" || (len(config.WebhookSecret) == 0 && config.TokenSource == nil) {
		return nil, domain.NewError(domain.CodeValidation, errors.New("github adapter requires an API URL and at least one authentication capability"))
	}
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("github adapter API URL is invalid"))
	}
	return &Adapter{apiBaseURL: baseURL, webhookSecret: append([]byte(nil), config.WebhookSecret...), tokenSource: config.TokenSource, client: newHTTPClient()}, nil
}

func (a *Adapter) DecodeWebhook(headers http.Header, body []byte) (forge.ForgeEvent, error) {
	if len(body) == 0 || len(body) > maxWebhookBytes {
		return forge.ForgeEvent{}, domain.NewError(domain.CodeValidation, errors.New("github webhook payload size is invalid"))
	}
	if err := verifyWebhookSignature(a.webhookSecret, headers.Get("X-Hub-Signature-256"), body); err != nil {
		return forge.ForgeEvent{}, err
	}
	deliveryID := strings.TrimSpace(headers.Get("X-GitHub-Delivery"))
	if deliveryID == "" || len(deliveryID) > 128 || strings.IndexFunc(deliveryID, func(r rune) bool { return r <= ' ' }) >= 0 {
		return forge.ForgeEvent{}, domain.NewError(domain.CodeValidation, errors.New("github webhook delivery ID is invalid"))
	}
	if headers.Get("X-GitHub-Event") != "pull_request" {
		return forge.ForgeEvent{Provider: "github", DeliveryID: deliveryID, Ignored: true}, nil
	}
	var payload webhookPayload
	if err := decodeJSON(body, &payload); err != nil {
		return forge.ForgeEvent{}, domain.NewError(domain.CodeValidation, errors.New("github webhook payload is invalid"))
	}
	if !validForgeAction(payload.Action) {
		return forge.ForgeEvent{Provider: "github", DeliveryID: deliveryID, Ignored: true}, nil
	}
	key, err := domain.ParseChangeKey("github:" + payload.Repository.FullName + "#" + strconv.Itoa(payload.Number))
	if err != nil || payload.Installation.ID < 1 || !validSHA(payload.PullRequest.Base.SHA) || !validSHA(payload.PullRequest.Head.SHA) || payload.PullRequest.Base.Ref == "" || payload.PullRequest.Head.Ref == "" {
		return forge.ForgeEvent{}, domain.NewError(domain.CodeValidation, errors.New("github pull request webhook is incomplete"))
	}
	return forge.ForgeEvent{Provider: "github", DeliveryID: deliveryID, Action: payload.Action, Change: key, BaseRef: payload.PullRequest.Base.Ref, BaseSHA: strings.ToLower(payload.PullRequest.Base.SHA), HeadRef: payload.PullRequest.Head.Ref, HeadSHA: strings.ToLower(payload.PullRequest.Head.SHA), InstallationID: payload.Installation.ID}, nil
}

func (a *Adapter) GetChange(ctx context.Context, key domain.ChangeKey) (forge.Change, error) {
	if err := validateGitHubChangeKey(key); err != nil {
		return forge.Change{}, err
	}
	token, err := a.installationToken(ctx, key.Repository, map[string]string{"contents": "read", "pull_requests": "read"})
	if err != nil {
		return forge.Change{}, err
	}
	target := a.apiBaseURL + "/repos/" + escapeRepository(key.Repository) + "/pulls/" + strconv.Itoa(key.Number)
	var response pullRequestResponse
	if err := a.callJSON(ctx, http.MethodGet, target, token.Value, nil, &response, http.StatusOK); err != nil {
		return forge.Change{}, err
	}
	if response.Number != key.Number || !validSHA(response.Base.SHA) || !validSHA(response.Head.SHA) || response.Base.Ref == "" || response.Head.Ref == "" {
		return forge.Change{}, domain.NewError(domain.CodeValidation, errors.New("github pull request response is incomplete"))
	}
	state := forge.ChangeOpen
	if response.Merged {
		state = forge.ChangeMerged
		if !validSHA(response.MergeCommitSHA) {
			return forge.Change{}, domain.NewError(domain.CodeValidation, errors.New("merged github pull request has no valid merge SHA"))
		}
	} else if response.State == "closed" {
		state = forge.ChangeClosed
	} else if response.State != "open" {
		return forge.Change{}, domain.NewError(domain.CodeValidation, errors.New("github pull request state is invalid"))
	}
	return forge.Change{Key: key, State: state, BaseRef: response.Base.Ref, BaseSHA: strings.ToLower(response.Base.SHA), HeadRef: response.Head.Ref, HeadSHA: strings.ToLower(response.Head.SHA), Merged: response.Merged, MergeSHA: strings.ToLower(response.MergeCommitSHA)}, nil
}

func (a *Adapter) UpsertCheck(ctx context.Context, input forge.CheckInput) error {
	if err := validateGitHubChangeKey(input.Change); err != nil {
		return err
	}
	if !validSHA(input.HeadSHA) || len(input.Summary) > maxCheckSummaryBytes || input.Summary == "" {
		return domain.NewError(domain.CodeValidation, errors.New("github check input is invalid"))
	}
	status, conclusion, title, err := checkMapping(input.State)
	if err != nil {
		return err
	}
	token, err := a.installationToken(ctx, input.Change.Repository, map[string]string{"checks": "write"})
	if err != nil {
		return err
	}
	payload := checkRunRequest{Name: checkName, HeadSHA: strings.ToLower(input.HeadSHA), Status: status, Conclusion: conclusion, DetailsURL: input.PlanURL, ExternalID: changeKeyString(input.Change), Output: checkRunOutput{Title: title, Summary: input.Summary}}
	method := http.MethodPost
	target := a.apiBaseURL + "/repos/" + escapeRepository(input.Change.Repository) + "/check-runs"
	if input.CheckRunID > 0 {
		method = http.MethodPatch
		target += "/" + strconv.FormatInt(input.CheckRunID, 10)
		payload.Name = ""
		payload.HeadSHA = ""
	}
	return a.callJSON(ctx, method, target, token.Value, payload, nil, http.StatusCreated, http.StatusOK)
}

func (a *Adapter) ReconcileCheck(ctx context.Context, input forge.CheckInput) (forge.CheckPublication, error) {
	if err := validateGitHubChangeKey(input.Change); err != nil {
		return forge.CheckPublication{}, err
	}
	if !validSHA(input.HeadSHA) || len(input.Summary) > maxCheckSummaryBytes || input.Summary == "" {
		return forge.CheckPublication{}, domain.NewError(domain.CodeValidation, errors.New("github check input is invalid"))
	}
	status, conclusion, title, err := checkMapping(input.State)
	if err != nil {
		return forge.CheckPublication{}, err
	}
	token, err := a.installationToken(ctx, input.Change.Repository, map[string]string{"checks": "write"})
	if err != nil {
		return forge.CheckPublication{}, err
	}
	externalID := checkExternalID(input.Change, input.HeadSHA)
	listTarget := a.apiBaseURL + "/repos/" + escapeRepository(input.Change.Repository) + "/commits/" + url.PathEscape(strings.ToLower(input.HeadSHA)) + "/check-runs?check_name=" + url.QueryEscape(checkName) + "&filter=all&per_page=100"
	var listed checkRunsResponse
	if err := a.callJSON(ctx, http.MethodGet, listTarget, token.Value, nil, &listed, http.StatusOK); err != nil {
		return forge.CheckPublication{}, err
	}
	matching := make([]int64, 0, len(listed.CheckRuns))
	for _, run := range listed.CheckRuns {
		if run.ID > 0 && run.Name == checkName && strings.EqualFold(run.HeadSHA, input.HeadSHA) && run.ExternalID == externalID {
			matching = append(matching, run.ID)
		}
	}
	canonicalID := int64(0)
	for _, runID := range matching {
		if runID == input.CheckRunID {
			canonicalID = runID
			break
		}
	}
	if canonicalID == 0 {
		for _, runID := range matching {
			if canonicalID == 0 || runID < canonicalID {
				canonicalID = runID
			}
		}
	}
	baseTarget := a.apiBaseURL + "/repos/" + escapeRepository(input.Change.Repository) + "/check-runs"
	if canonicalID < 1 {
		request := checkRunRequest{Name: checkName, HeadSHA: strings.ToLower(input.HeadSHA), Status: status, Conclusion: conclusion, DetailsURL: input.PlanURL, ExternalID: externalID, Output: checkRunOutput{Title: title, Summary: input.Summary}}
		var created checkRunResponse
		if err := a.callJSON(ctx, http.MethodPost, baseTarget, token.Value, request, &created, http.StatusCreated); err != nil {
			return forge.CheckPublication{}, err
		}
		if created.ID < 1 {
			return forge.CheckPublication{}, providerUnavailableError()
		}
		canonicalID = created.ID
	} else {
		request := checkRunRequest{Status: status, Conclusion: conclusion, DetailsURL: input.PlanURL, ExternalID: externalID, Output: checkRunOutput{Title: title, Summary: input.Summary}}
		if err := a.callJSON(ctx, http.MethodPatch, baseTarget+"/"+strconv.FormatInt(canonicalID, 10), token.Value, request, nil, http.StatusOK); err != nil {
			return forge.CheckPublication{}, err
		}
	}
	for _, runID := range matching {
		if runID == canonicalID {
			continue
		}
		supersededStatus, supersededConclusion, supersededTitle, _ := checkMapping(forge.CheckSuperseded)
		request := checkRunRequest{Status: supersededStatus, Conclusion: supersededConclusion, ExternalID: externalID, Output: checkRunOutput{Title: supersededTitle, Summary: "A canonical Thread Keep check run superseded this duplicate."}}
		if err := a.callJSON(ctx, http.MethodPatch, baseTarget+"/"+strconv.FormatInt(runID, 10), token.Value, request, nil, http.StatusOK); err != nil {
			return forge.CheckPublication{}, err
		}
	}
	return forge.CheckPublication{CheckRunID: canonicalID}, nil
}

func (a *Adapter) CheckoutGrant(ctx context.Context, input forge.CheckoutGrantInput) (forge.CheckoutGrant, error) {
	if err := validateGitHubChangeKey(input.Change); err != nil {
		return forge.CheckoutGrant{}, err
	}
	token, err := a.installationToken(ctx, input.Change.Repository, map[string]string{"contents": "read"})
	if err != nil {
		return forge.CheckoutGrant{}, err
	}
	return forge.CheckoutGrant{Repository: input.Change.Repository, CloneURL: "https://github.com/" + input.Change.Repository + ".git", Token: token.Value, ExpiresAt: token.ExpiresAt}, nil
}

func (a *Adapter) installationToken(ctx context.Context, repository string, permissions map[string]string) (InstallationToken, error) {
	if a.tokenSource == nil {
		return InstallationToken{}, domain.NewError(domain.CodeAuth, errors.New("github app installation authentication is not configured"))
	}
	token, err := a.tokenSource.Token(ctx, repository, permissions)
	if err != nil {
		return InstallationToken{}, err
	}
	if token.Value == "" || token.ExpiresAt.Before(time.Now().Add(time.Minute)) {
		return InstallationToken{}, domain.NewError(domain.CodeAuth, errors.New("github app installation credential is invalid or expiring"))
	}
	return token, nil
}

func (a *Adapter) callJSON(ctx context.Context, method, target, token string, requestBody any, responseBody any, success ...int) error {
	var body io.Reader
	if requestBody != nil {
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return domain.NewError(domain.CodeValidation, errors.New("serialize github API request"))
		}
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return providerUnavailableError()
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.client.Do(request)
	if err != nil {
		return providerUnavailableError()
	}
	defer response.Body.Close()
	for _, code := range success {
		if response.StatusCode == code {
			if responseBody == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxAPIResponseBytes))
				return nil
			}
			contents, err := io.ReadAll(io.LimitReader(response.Body, maxAPIResponseBytes+1))
			if err != nil || len(contents) > maxAPIResponseBytes || decodeJSON(contents, responseBody) != nil {
				return providerUnavailableError()
			}
			return nil
		}
	}
	return mapGitHubStatus(response.StatusCode)
}

func verifyWebhookSignature(secret []byte, presented string, body []byte) error {
	prefix := "sha256="
	if !strings.HasPrefix(presented, prefix) {
		return domain.NewError(domain.CodeAuth, errors.New("github webhook signature is missing or invalid"))
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(presented, prefix))
	if err != nil || len(decoded) != sha256.Size {
		return domain.NewError(domain.CodeAuth, errors.New("github webhook signature is missing or invalid"))
	}
	digest := hmac.New(sha256.New, secret)
	_, _ = digest.Write(body)
	if subtle.ConstantTimeCompare(decoded, digest.Sum(nil)) != 1 {
		return domain.NewError(domain.CodeAuth, errors.New("github webhook signature is missing or invalid"))
	}
	return nil
}

func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: githubDialTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = githubHeaderTimeout
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New(redirectRefusalMessage) }}
}

func decodeJSON(contents []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("response contains multiple JSON values")
	}
	return nil
}

func checkMapping(state forge.CheckState) (string, string, string, error) {
	switch state {
	case forge.CheckPlanning:
		return "in_progress", "", "Context planning", nil
	case forge.CheckReady:
		return "completed", "success", "Context plan ready", nil
	case forge.CheckReviewRequired:
		return "completed", "neutral", "Context review recommended", nil
	case forge.CheckBlocked:
		return "completed", "failure", "Context plan blocked", nil
	case forge.CheckSuperseded:
		return "completed", "cancelled", "Context plan superseded", nil
	default:
		return "", "", "", domain.NewError(domain.CodeValidation, errors.New("github check state is invalid"))
	}
}

func validateGitHubChangeKey(key domain.ChangeKey) error {
	canonical, err := domain.ParseChangeKey(changeKeyString(key))
	if err != nil || canonical != key || key.Provider != "github" || !strings.Contains(key.Repository, "/") {
		return domain.NewError(domain.CodeValidation, errors.New("github change key is invalid"))
	}
	return nil
}

func changeKeyString(key domain.ChangeKey) string {
	return key.Provider + ":" + key.Repository + "#" + strconv.Itoa(key.Number)
}

func checkExternalID(key domain.ChangeKey, headSHA string) string {
	return changeKeyString(key) + "@" + strings.ToLower(headSHA)
}

func validForgeAction(action forge.ForgeAction) bool {
	return action == forge.ForgeActionOpened || action == forge.ForgeActionReopened || action == forge.ForgeActionSynchronize || action == forge.ForgeActionClosed
}

func validSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func escapeRepository(repository string) string {
	parts := strings.Split(repository, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}

func mapGitHubStatus(status int) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return domain.NewError(domain.CodeAuth, errors.New("github app is not authorized for the requested operation"))
	case http.StatusNotFound:
		return domain.NewError(domain.CodeEntityNotFound, errors.New("github resource was not found or is not visible to the app"))
	default:
		return providerUnavailableError()
	}
}

func providerUnavailableError() error {
	return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("github provider is unavailable"))
}
