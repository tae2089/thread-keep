package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/tae2089/thread-keep/internal/domain"
)

const maxCandidatePublicationBytes = 1 << 20

type CandidatePublicationMetadata struct {
	Change              domain.ChangeKey `json:"change"`
	BaseSourceSHA       string           `json:"base_source_sha"`
	HeadSourceSHA       string           `json:"head_source_sha"`
	BaseContextCommitID string           `json:"base_context_commit_id"`
}

type CandidatePublicationRequest struct {
	Delta  domain.CandidateContextDelta `json:"delta"`
	Digest string                       `json:"digest"`
}

type CandidatePublicationResult struct {
	Digest    string `json:"digest"`
	Published bool   `json:"published"`
}

type CandidateTransport interface {
	CandidatePublicationMetadata(context.Context, domain.ChangeKey) (CandidatePublicationMetadata, error)
	PublishCandidate(context.Context, CandidatePublicationRequest) (CandidatePublicationResult, error)
}

var _ CandidateTransport = (*HTTP)(nil)

func (h *HTTP) CandidatePublicationMetadata(ctx context.Context, change domain.ChangeKey) (CandidatePublicationMetadata, error) {
	canonical, err := domain.ParseChangeKey(fmt.Sprintf("%s:%s#%d", change.Provider, change.Repository, change.Number))
	if err != nil || canonical != change {
		return CandidatePublicationMetadata{}, domain.NewError(domain.CodeValidation, errors.New("candidate publication change key is not canonical"))
	}
	response, err := h.do(ctx, http.MethodGet, h.candidateURL(change.Number), nil, "")
	if err != nil {
		return CandidatePublicationMetadata{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return CandidatePublicationMetadata{}, remoteResponseError(response)
	}
	var metadata CandidatePublicationMetadata
	if err := decodeCandidateJSON(response.Body, &metadata); err != nil {
		return CandidatePublicationMetadata{}, err
	}
	if metadata.Change != change || metadata.BaseSourceSHA == "" || metadata.HeadSourceSHA == "" || metadata.BaseContextCommitID == "" {
		return CandidatePublicationMetadata{}, domain.NewError(domain.CodeValidation, errors.New("remote candidate publication metadata is incomplete or mismatched"))
	}
	if _, err := domain.NormalizeContextCommitID(metadata.BaseContextCommitID); err != nil {
		return CandidatePublicationMetadata{}, err
	}
	return metadata, nil
}

func (h *HTTP) PublishCandidate(ctx context.Context, request CandidatePublicationRequest) (CandidatePublicationResult, error) {
	delta, err := domain.NormalizeCandidateContextDelta(request.Delta)
	if err != nil {
		return CandidatePublicationResult{}, err
	}
	digest, err := domain.CandidateContextDigest(delta)
	if err != nil || digest != request.Digest {
		return CandidatePublicationResult{}, domain.NewError(domain.CodeValidation, errors.New("candidate publication digest does not match its delta"))
	}
	payload, err := json.Marshal(CandidatePublicationRequest{Delta: delta, Digest: digest})
	if err != nil {
		return CandidatePublicationResult{}, storageError("serialize candidate publication request", err)
	}
	if len(payload) > maxCandidatePublicationBytes {
		return CandidatePublicationResult{}, domain.NewError(domain.CodeValidation, errors.New("candidate publication request exceeds the size limit"))
	}
	response, err := h.do(ctx, http.MethodPut, h.candidateURL(delta.Change.Number), payload, "application/json")
	if err != nil {
		return CandidatePublicationResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return CandidatePublicationResult{}, remoteResponseError(response)
	}
	var result CandidatePublicationResult
	if err := decodeCandidateJSON(response.Body, &result); err != nil {
		return CandidatePublicationResult{}, err
	}
	wantPublished := response.StatusCode == http.StatusCreated
	if result.Digest != digest || result.Published != wantPublished {
		return CandidatePublicationResult{}, domain.NewError(domain.CodeValidation, errors.New("remote candidate publication result is inconsistent"))
	}
	return result, nil
}

func (h *HTTP) candidateURL(number int) string {
	return h.baseURL + "/pull-requests/" + strconv.Itoa(number) + "/candidate"
}

func decodeCandidateJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxCandidatePublicationBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return domain.NewError(domain.CodeValidation, fmt.Errorf("decode candidate publication response: %w", err))
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.NewError(domain.CodeValidation, errors.New("candidate publication response must contain one JSON value"))
	}
	return nil
}
