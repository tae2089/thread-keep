package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
)

const (
	routePrefix                                = "/v1/repositories/"
	clusterSecretHeader                        = "X-Thread-Keep-Cluster-Secret"
	maxObjectRequestBytes                      = 64 << 20
	maxObjectListResponseBytes                 = 1 << 20
	maxControlRequestBytes                     = 8 << 10
	githubDialTimeout                          = 10 * time.Second
	githubHeaderTimeout                        = 30 * time.Second
	redirectRefusalMessage                     = "remote redirects are not followed"
	limitedJSONFirstValue      limitedJSONMode = iota
	limitedJSONSingleValue
)

type limitedJSONMode uint8

type Config struct {
	GitHubAPIBaseURL string                      `json:"github_api_base_url"`
	GitHubApp        *GitHubAppConfig            `json:"github_app,omitempty"`
	Repositories     map[string]RepositoryConfig `json:"repositories"`
	Cluster          *ClusterConfig              `json:"cluster,omitempty"`
	GC               *GCConfig                   `json:"gc,omitempty"`
}

type GitHubAppConfig struct {
	AppID          int64 `json:"app_id"`
	InstallationID int64 `json:"installation_id"`
}

type RepositoryConfig struct {
	GitHubOwner         string          `json:"github_owner"`
	GitHubRepo          string          `json:"github_repo"`
	ContextRepositoryID string          `json:"context_repository_id,omitempty"`
	Planning            *PlanningConfig `json:"planning,omitempty"`
}

type PlanningConfig struct {
	Enabled          bool     `json:"enabled"`
	TargetBranches   []string `json:"target_branches,omitempty"`
	CheckMode        string   `json:"check_mode,omitempty"`
	AutomaticLanding bool     `json:"automatic_landing"`
	ContextSchema    int      `json:"context_schema,omitempty"`
	MaxAttempts      int      `json:"max_attempts,omitempty"`
}

type ClusterConfig struct {
	NodeID             string      `json:"node_id"`
	AdvertiseURL       string      `json:"advertise_url"`
	Membership         string      `json:"membership,omitempty"`
	Swim               *SwimConfig `json:"swim,omitempty"`
	ReplicationFactor  int         `json:"replication_factor"`
	HeartbeatSeconds   int         `json:"heartbeat_seconds"`
	TTLSeconds         int         `json:"ttl_seconds"`
	AntiEntropySeconds int         `json:"anti_entropy_seconds"`
}

type SwimConfig struct {
	BindAddr string   `json:"bind_addr"`
	Seeds    []string `json:"seeds,omitempty"`
}

type Server struct {
	store         Storage
	direct        Storage
	clusterSecret string
	config        Config
	github        *http.Client
	coordinator   *Coordinator
}

type httpError struct {
	status  int
	code    domain.ErrorCode
	message string
}

type casRequest struct {
	Expected remote.Ref `json:"expected"`
	Next     remote.Ref `json:"next"`
}

func (e *httpError) Error() string { return e.message }

func NewHandler(store Storage, config Config) (http.Handler, error) {
	return newServer(store, store, "", config)
}

func NewCoordinatorHandler(store Storage, coordinator *Coordinator, config Config) (http.Handler, error) {
	if coordinator == nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("coordinator handler requires a coordinator"))
	}
	handler, err := newServer(store, store, "", config)
	if err != nil {
		return nil, err
	}
	server := handler.(*Server)
	server.coordinator = coordinator
	return server, nil
}

func NewClusterHandler(direct, replicated Storage, clusterSecret string, config Config) (http.Handler, error) {
	if strings.TrimSpace(clusterSecret) == "" {
		return nil, domain.NewError(domain.CodeValidation, errors.New("cluster mode requires a non-empty cluster secret"))
	}
	return newServer(direct, replicated, clusterSecret, config)
}

func NewClusterCoordinatorHandler(direct, replicated Storage, clusterSecret string, coordinator *Coordinator, config Config) (http.Handler, error) {
	if coordinator == nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("cluster coordinator handler requires a coordinator"))
	}
	handler, err := NewClusterHandler(direct, replicated, clusterSecret, config)
	if err != nil {
		return nil, err
	}
	server := handler.(*Server)
	server.coordinator = coordinator
	return server, nil
}

func newGitHubClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   githubDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.ResponseHeaderTimeout = githubHeaderTimeout
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New(redirectRefusalMessage)
		},
	}
}

func newServer(direct, replicated Storage, clusterSecret string, config Config) (http.Handler, error) {
	if direct == nil || replicated == nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("server storage must be configured"))
	}
	if strings.TrimSpace(config.GitHubAPIBaseURL) == "" {
		return nil, domain.NewError(domain.CodeValidation, errors.New("server configuration must set the GitHub API base URL"))
	}
	for repositoryID, repository := range config.Repositories {
		if repositoryID == "" || repositoryID == "." || repositoryID == ".." || strings.ContainsAny(repositoryID, `/\`) {
			return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("repository id %q is not a safe storage segment", repositoryID))
		}
		if repository.GitHubOwner == "" || repository.GitHubRepo == "" {
			return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("repository %q must map to a GitHub owner and repository", repositoryID))
		}
	}
	config.GitHubAPIBaseURL = strings.TrimRight(strings.TrimSpace(config.GitHubAPIBaseURL), "/")
	return &Server{store: replicated, direct: direct, clusterSecret: clusterSecret, config: config, github: newGitHubClient()}, nil
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if err := s.serve(writer, request); err != nil {
		writeError(writer, err)
	}
}

func (s *Server) serve(writer http.ResponseWriter, request *http.Request) error {
	if s.coordinator != nil {
		handled, err := s.serveCoordinator(writer, request)
		if handled || err != nil {
			return err
		}
	}
	rest, ok := strings.CutPrefix(request.URL.EscapedPath(), routePrefix)
	if !ok {
		return &httpError{status: http.StatusNotFound, code: domain.CodeValidation, message: "unknown route"}
	}
	segments := strings.Split(rest, "/")
	if len(segments) != 2 && len(segments) != 3 {
		return &httpError{status: http.StatusNotFound, code: domain.CodeValidation, message: "unknown route"}
	}
	repositoryID, err := url.PathUnescape(segments[0])
	if err != nil {
		return &httpError{status: http.StatusNotFound, code: domain.CodeValidation, message: "unknown route"}
	}
	repository, ok := s.config.Repositories[repositoryID]
	if !ok {
		return &httpError{status: http.StatusNotFound, code: domain.CodeValidation, message: fmt.Sprintf("repository %q is not configured on this context remote", repositoryID)}
	}
	var write bool
	switch request.Method {
	case http.MethodGet:
		write = false
	case http.MethodPut:
		write = true
	default:
		return &httpError{status: http.StatusMethodNotAllowed, code: domain.CodeValidation, message: "method is not allowed"}
	}
	store := s.store
	cluster, err := s.authorizeCluster(request)
	if err != nil {
		return err
	}
	if len(segments) == 2 {
		if segments[1] != "objects" || !cluster {
			return &httpError{status: http.StatusNotFound, code: domain.CodeValidation, message: "unknown route"}
		}
		if write {
			return &httpError{status: http.StatusMethodNotAllowed, code: domain.CodeValidation, message: "method is not allowed"}
		}
		return s.serveObjectList(writer, request, s.direct, repositoryID)
	}
	name, err := url.PathUnescape(segments[2])
	if err != nil {
		return &httpError{status: http.StatusNotFound, code: domain.CodeValidation, message: "unknown route"}
	}
	if cluster {
		store = s.direct
	} else if err := s.authorize(request.Context(), request.Header.Get("Authorization"), repository, write); err != nil {
		return err
	}
	switch segments[1] {
	case "refs":
		if write {
			return s.serveRefCAS(writer, request, store, repositoryID, name)
		}
		return s.serveRefRead(writer, request, store, repositoryID, name)
	case "objects":
		if write {
			return s.serveObjectPublish(writer, request, store, repositoryID, name)
		}
		return s.serveObjectRead(writer, request, store, repositoryID, name)
	default:
		return &httpError{status: http.StatusNotFound, code: domain.CodeValidation, message: "unknown route"}
	}
}

func (s *Server) authorizeCluster(request *http.Request) (bool, error) {
	presented := request.Header.Get(clusterSecretHeader)
	if s.clusterSecret == "" || presented == "" {
		return false, nil
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(s.clusterSecret)) != 1 {
		return false, &httpError{status: http.StatusForbidden, code: domain.CodeAuth, message: "cluster credential is not authorized"}
	}
	return true, nil
}

func (s *Server) authorize(ctx context.Context, authorization string, repository RepositoryConfig, write bool) error {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return &httpError{status: http.StatusUnauthorized, code: domain.CodeAuth, message: "request must carry a bearer token"}
	}
	target := s.config.GitHubAPIBaseURL + "/repos/" + url.PathEscape(repository.GitHubOwner) + "/" + url.PathEscape(repository.GitHubRepo)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return &httpError{status: http.StatusBadGateway, code: domain.CodeLocalStorage, message: "github verification is unavailable"}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := s.github.Do(request)
	if err != nil {
		return &httpError{status: http.StatusBadGateway, code: domain.CodeLocalStorage, message: "github verification is unavailable"}
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		var repositoryInfo struct {
			Permissions struct {
				Push bool `json:"push"`
				Pull bool `json:"pull"`
			} `json:"permissions"`
		}
		if err := decodeJSONLimited(response.Body, &repositoryInfo, limitedJSONFirstValue); err != nil {
			return &httpError{status: http.StatusBadGateway, code: domain.CodeLocalStorage, message: "github verification is unavailable"}
		}
		if write && !repositoryInfo.Permissions.Push {
			return &httpError{status: http.StatusForbidden, code: domain.CodeAuth, message: "token lacks push permission for the mapped GitHub repository"}
		}
		if !write && !repositoryInfo.Permissions.Pull {
			return &httpError{status: http.StatusForbidden, code: domain.CodeAuth, message: "token lacks pull permission for the mapped GitHub repository"}
		}
		return nil
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return &httpError{status: http.StatusForbidden, code: domain.CodeAuth, message: "token is not authorized for the mapped GitHub repository"}
	default:
		return &httpError{status: http.StatusBadGateway, code: domain.CodeLocalStorage, message: "github verification is unavailable"}
	}
}

func (s *Server) serveRefRead(writer http.ResponseWriter, request *http.Request, store Storage, repositoryID, refName string) error {
	ref, err := store.ReadRef(request.Context(), repositoryID, refName)
	if err != nil {
		return err
	}
	return writeJSON(writer, http.StatusOK, ref)
}

func (s *Server) serveRefCAS(writer http.ResponseWriter, request *http.Request, store Storage, repositoryID, refName string) error {
	var payload casRequest
	if err := decodeJSONLimited(request.Body, &payload, limitedJSONFirstValue); err != nil {
		return &httpError{status: http.StatusBadRequest, code: domain.CodeValidation, message: "decode compare-and-swap request"}
	}
	var confirmed remote.Ref
	var err error
	if s.coordinator != nil {
		contents, readErr := store.ReadObject(request.Context(), repositoryID, payload.Next.CommitID)
		if readErr != nil {
			return readErr
		}
		var object domain.ContextObject
		if decodeErr := json.Unmarshal(contents, &object); decodeErr != nil {
			return domain.NewError(domain.CodeValidation, errors.New("next context object is invalid"))
		}
		if len(object.LandingReceipts) != 0 {
			if object.RepositoryID != repositoryID || object.RefName != refName {
				return domain.NewError(domain.CodeValidation, errors.New("landing receipt object does not match the requested repository and ref"))
			}
			confirmed, err = s.coordinator.refService.Advance(request.Context(), RefAdvanceInput{Expected: payload.Expected, Next: payload.Next, Object: object})
		} else {
			confirmed, err = store.CompareAndSwapRef(request.Context(), repositoryID, refName, payload.Expected, payload.Next)
		}
	} else {
		confirmed, err = store.CompareAndSwapRef(request.Context(), repositoryID, refName, payload.Expected, payload.Next)
	}
	if err != nil {
		return err
	}
	return writeJSON(writer, http.StatusOK, confirmed)
}

func (s *Server) serveObjectList(writer http.ResponseWriter, request *http.Request, store Storage, repositoryID string) error {
	ids, err := store.ListObjects(request.Context(), repositoryID)
	if err != nil {
		return err
	}
	return writeJSON(writer, http.StatusOK, ids)
}

func (s *Server) serveObjectRead(writer http.ResponseWriter, request *http.Request, store Storage, repositoryID, objectID string) error {
	contents, err := store.ReadObject(request.Context(), repositoryID, objectID)
	if err != nil {
		if isMissingObjectError(err) {
			return &httpError{status: http.StatusNotFound, code: domain.CodeObjectMissing, message: err.Error()}
		}
		return err
	}
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(contents)
	return nil
}

func (s *Server) serveObjectPublish(writer http.ResponseWriter, request *http.Request, store Storage, repositoryID, objectID string) error {
	contents, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxObjectRequestBytes))
	if err != nil {
		return &httpError{status: http.StatusBadRequest, code: domain.CodeValidation, message: "object upload exceeds the size limit or was interrupted"}
	}
	created, err := store.PublishObject(request.Context(), repositoryID, objectID, contents)
	if err != nil {
		return err
	}
	if created {
		writer.WriteHeader(http.StatusCreated)
		return nil
	}
	writer.WriteHeader(http.StatusOK)
	return nil
}

func decodeJSONLimited(reader io.Reader, target any, mode limitedJSONMode) error {
	return decodeJSONLimitedTo(reader, target, mode, maxControlRequestBytes)
}

func decodeJSONLimitedTo(reader io.Reader, target any, mode limitedJSONMode, maximum int64) error {
	if mode == limitedJSONFirstValue {
		return json.NewDecoder(io.LimitReader(reader, maximum)).Decode(target)
	}
	contents, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return err
	}
	if int64(len(contents)) > maximum {
		return errors.New("JSON input exceeds the size limit")
	}
	return json.Unmarshal(contents, target)
}

func writeJSON(writer http.ResponseWriter, status int, payload any) error {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
	return nil
}

func writeError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := domain.CodeOf(err)
	var typed *httpError
	if errors.As(err, &typed) {
		status = typed.status
		code = typed.code
	} else {
		switch code {
		case domain.CodeValidation:
			status = http.StatusBadRequest
		case domain.CodeRemoteConflict:
			status = http.StatusConflict
		case domain.CodeAuth:
			status = http.StatusForbidden
		case domain.CodeEntityNotFound, domain.CodeObjectMissing:
			status = http.StatusNotFound
		case domain.CodeStaleWorkingSet, domain.CodeConcurrentUpdate:
			status = http.StatusConflict
		case domain.CodeCoverageIncomplete:
			status = http.StatusUnprocessableEntity
		case domain.CodeBusy:
			status = http.StatusServiceUnavailable
		case "":
			code = domain.CodeLocalStorage
		}
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(struct {
		Code    domain.ErrorCode `json:"code"`
		Message string           `json:"message"`
	}{Code: code, Message: err.Error()})
}
