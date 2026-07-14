package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

const (
	TokenEnvironmentVariable = "THREAD_KEEP_REMOTE_TOKEN"

	maxObjectResponseBytes = 64 << 20
	maxErrorResponseBytes  = 8 << 10
	remoteDialTimeout      = 10 * time.Second
	remoteHeaderTimeout    = 30 * time.Second
	redirectRefusalMessage = "remote redirects are not followed"
)

type HTTP struct {
	baseURL string
	token   string
	client  *http.Client
}

type casRequest struct {
	Expected Ref `json:"expected"`
	Next     Ref `json:"next"`
}

var _ Transport = (*HTTP)(nil)

var remoteErrorCodes = map[domain.ErrorCode]bool{
	domain.CodeValidation:         true,
	domain.CodeObjectMissing:      true,
	domain.CodeRemoteConflict:     true,
	domain.CodeAuth:               true,
	domain.CodeLocalStorage:       true,
	domain.CodeEntityNotFound:     true,
	domain.CodeStaleWorkingSet:    true,
	domain.CodeConcurrentUpdate:   true,
	domain.CodeCoverageIncomplete: true,
	domain.CodeBusy:               true,
}

func NewHTTP(baseURL, token string) *HTTP {
	return &HTTP{baseURL: baseURL, token: token, client: newHTTPClient()}
}

func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   remoteDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.ResponseHeaderTimeout = remoteHeaderTimeout
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New(redirectRefusalMessage)
		},
	}
}

func (h *HTTP) ReadObject(ctx context.Context, id string) ([]byte, error) {
	id, err := domain.NormalizeContextCommitID(id)
	if err != nil {
		return nil, err
	}
	response, err := h.do(ctx, http.MethodGet, h.objectURL(id), nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, remoteResponseError(response)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxObjectResponseBytes+1))
	if err != nil {
		return nil, storageError("read remote object response", err)
	}
	if len(contents) > maxObjectResponseBytes {
		return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("remote object %s exceeds the response size limit", id))
	}
	if err := validateObjectBytes(id, contents); err != nil {
		return nil, err
	}
	return contents, nil
}

func (h *HTTP) PublishObject(ctx context.Context, id string, contents []byte) (bool, error) {
	id, err := domain.NormalizeContextCommitID(id)
	if err != nil {
		return false, err
	}
	if err := validateObjectBytes(id, contents); err != nil {
		return false, err
	}
	response, err := h.do(ctx, http.MethodPut, h.objectURL(id), contents, "application/octet-stream")
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusCreated:
		return true, nil
	case http.StatusOK:
		return false, nil
	default:
		return false, remoteResponseError(response)
	}
}

func (h *HTTP) ReadRef(ctx context.Context, refName string) (Ref, error) {
	if refName == "" {
		return Ref{}, domain.NewError(domain.CodeValidation, errors.New("remote ref name must not be empty"))
	}
	response, err := h.do(ctx, http.MethodGet, h.refURL(refName), nil, "")
	if err != nil {
		return Ref{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Ref{}, remoteResponseError(response)
	}
	ref, err := decodeRef(response.Body, refName)
	if err != nil {
		return Ref{}, err
	}
	if ref.Version == 0 {
		return Ref{RefName: refName}, nil
	}
	if ref.CommitID == "" || ref.SourceSHA == "" || ref.Version < 1 {
		return Ref{}, domain.NewError(domain.CodeValidation, errors.New("remote ref is invalid"))
	}
	if _, err := domain.NormalizeContextCommitID(ref.CommitID); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

func (h *HTTP) CompareAndSwapRef(ctx context.Context, refName string, expected, next Ref) (Ref, error) {
	if expected.RefName != refName || next.RefName != refName || next.CommitID == "" || next.SourceSHA == "" || next.Version < 1 {
		return Ref{}, domain.NewError(domain.CodeValidation, errors.New("remote ref compare-and-swap input is invalid"))
	}
	payload, err := json.Marshal(casRequest{Expected: expected, Next: next})
	if err != nil {
		return Ref{}, storageError("serialize remote ref compare-and-swap request", err)
	}
	response, err := h.do(ctx, http.MethodPut, h.refURL(refName), payload, "application/json")
	if err != nil {
		return Ref{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Ref{}, remoteResponseError(response)
	}
	confirmed, err := decodeRef(response.Body, refName)
	if err != nil {
		return Ref{}, err
	}
	if confirmed != next {
		return Ref{}, domain.NewError(domain.CodeValidation, errors.New("remote confirmed a different ref than requested"))
	}
	return confirmed, nil
}

func (h *HTTP) do(ctx context.Context, method, target string, body []byte, contentType string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, storageError("build remote request", err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if h.token != "" {
		request.Header.Set("Authorization", "Bearer "+h.token)
	}
	response, err := h.client.Do(request)
	if err != nil {
		return nil, storageError("call remote", err)
	}
	return response, nil
}

func (h *HTTP) objectURL(id string) string {
	return h.baseURL + "/objects/" + url.PathEscape(id)
}

func (h *HTTP) refURL(refName string) string {
	return h.baseURL + "/refs/" + url.PathEscape(refName)
}

func decodeRef(body io.Reader, refName string) (Ref, error) {
	var ref Ref
	if err := json.NewDecoder(io.LimitReader(body, maxErrorResponseBytes)).Decode(&ref); err != nil {
		return Ref{}, domain.NewError(domain.CodeValidation, fmt.Errorf("decode remote ref: %w", err))
	}
	if ref.RefName != refName {
		return Ref{}, domain.NewError(domain.CodeValidation, errors.New("remote ref name does not match the requested ref"))
	}
	return ref, nil
}

func remoteResponseError(response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorResponseBytes))
	if err == nil {
		var payload struct {
			Code    domain.ErrorCode `json:"code"`
			Message string           `json:"message"`
		}
		if json.Unmarshal(body, &payload) == nil && remoteErrorCodes[payload.Code] && payload.Message != "" {
			return domain.NewError(payload.Code, errors.New(payload.Message))
		}
	}
	return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("remote returned status %d", response.StatusCode))
}
