package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
)

const maxCoordinatorRequestBytes = 1 << 20

func (s *Server) serveCoordinator(writer http.ResponseWriter, request *http.Request) (bool, error) {
	rest, ok := strings.CutPrefix(request.URL.EscapedPath(), routePrefix)
	if !ok {
		return false, nil
	}
	segments := strings.Split(rest, "/")
	if len(segments) < 2 {
		return false, nil
	}
	repositoryKey, err := url.PathUnescape(segments[0])
	if err != nil {
		return true, &httpError{status: http.StatusNotFound, code: domain.CodeValidation, message: "unknown route"}
	}
	repositoryConfig, configured := s.config.Repositories[repositoryKey]
	if !configured {
		return false, nil
	}
	isCoordinatorRoute := len(segments) == 2 && segments[1] == "capabilities" ||
		len(segments) == 2 && segments[1] == "landings" ||
		len(segments) == 4 && segments[1] == "pull-requests" && (segments[3] == "candidate" || segments[3] == "plan") ||
		len(segments) == 3 && (segments[1] == "plans" || segments[1] == "landings") ||
		len(segments) == 4 && segments[1] == "landings" && segments[3] == "recovery"
	if !isCoordinatorRoute {
		return false, nil
	}
	write := request.Method == http.MethodPut || request.Method == http.MethodPost || len(segments) == 4 && segments[1] == "landings" && segments[3] == "recovery"
	if err := s.authorize(request.Context(), request.Header.Get("Authorization"), repositoryConfig, write); err != nil {
		return true, err
	}
	switch {
	case len(segments) == 2 && segments[1] == "capabilities" && request.Method == http.MethodGet:
		repository, err := s.coordinator.repository(repositoryKey)
		if err != nil {
			return true, err
		}
		features := []string{"candidate_context_v2", "context_landing_recovery", "context_planning"}
		if repository.AutomaticLanding {
			features = append(features, "automatic_context_landing")
		}
		return true, writeJSON(writer, http.StatusOK, map[string]any{"context_object_versions": []int{1, 2, 3, 4}, "context_object_write_version": 4, "candidate_delta_versions": []int{1, 2}, "features": features})
	case len(segments) == 4 && segments[1] == "pull-requests" && segments[3] == "candidate":
		number, err := strconv.Atoi(segments[2])
		if err != nil || number < 1 {
			return true, domain.NewError(domain.CodeValidation, errors.New("pull request number is invalid"))
		}
		if request.Method == http.MethodGet {
			metadata, err := s.coordinator.CandidateMetadata(request.Context(), repositoryKey, number)
			if err != nil {
				return true, err
			}
			return true, writeJSON(writer, http.StatusOK, metadata)
		}
		if request.Method == http.MethodPut {
			var publication remote.CandidatePublicationRequest
			if err := decodeCoordinatorJSON(writer, request, &publication); err != nil {
				return true, err
			}
			if publication.Delta.Change.Number != number {
				return true, domain.NewError(domain.CodeValidation, errors.New("candidate route and payload numbers do not match"))
			}
			result, err := s.coordinator.PublishCandidate(request.Context(), repositoryKey, publication)
			if err != nil {
				return true, err
			}
			status := http.StatusOK
			if result.Published {
				status = http.StatusCreated
			}
			return true, writeJSON(writer, status, result)
		}
		return true, &httpError{status: http.StatusMethodNotAllowed, code: domain.CodeValidation, message: "method is not allowed"}
	case len(segments) == 4 && segments[1] == "pull-requests" && segments[3] == "plan" && request.Method == http.MethodGet:
		number, err := strconv.Atoi(segments[2])
		if err != nil || number < 1 {
			return true, domain.NewError(domain.CodeValidation, errors.New("pull request number is invalid"))
		}
		repository, err := s.coordinator.repository(repositoryKey)
		if err != nil {
			return true, err
		}
		plan, err := s.coordinator.PlanForChange(request.Context(), repositoryKey, domain.ChangeKey{Provider: "github", Repository: repository.ForgeRepository, Number: number})
		if err != nil {
			return true, err
		}
		return true, writeJSON(writer, http.StatusOK, plan)
	case len(segments) == 3 && segments[1] == "plans" && request.Method == http.MethodGet:
		planID, err := url.PathUnescape(segments[2])
		if err != nil {
			return true, domain.NewError(domain.CodeValidation, errors.New("plan ID is invalid"))
		}
		plan, err := s.coordinator.Plan(request.Context(), repositoryKey, planID)
		if err != nil {
			return true, err
		}
		return true, writeJSON(writer, http.StatusOK, plan)
	case len(segments) == 2 && segments[1] == "landings" && request.Method == http.MethodGet:
		landings, err := s.coordinator.Landings(request.Context(), repositoryKey)
		if err != nil {
			return true, err
		}
		return true, writeJSON(writer, http.StatusOK, landings)
	case len(segments) == 3 && segments[1] == "landings" && request.Method == http.MethodGet:
		landing, err := s.coordinator.Landing(request.Context(), repositoryKey, segments[2])
		if err != nil {
			return true, err
		}
		return true, writeJSON(writer, http.StatusOK, landing)
	case len(segments) == 4 && segments[1] == "landings" && segments[3] == "recovery" && request.Method == http.MethodGet:
		bundle, err := s.coordinator.LandingRecovery(request.Context(), repositoryKey, segments[2])
		if err != nil {
			return true, err
		}
		return true, writeJSON(writer, http.StatusOK, bundle)
	default:
		return false, nil
	}
}

func decodeCoordinatorJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxCoordinatorRequestBytes))
	if err != nil {
		return &httpError{status: http.StatusRequestEntityTooLarge, code: domain.CodeValidation, message: "coordinator request exceeds the size limit or was interrupted"}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return domain.NewError(domain.CodeValidation, errors.New("coordinator request must contain one valid JSON value"))
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return domain.NewError(domain.CodeValidation, errors.New("coordinator request must contain one valid JSON value"))
	}
	return nil
}
