package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/tae2089/thread-keep/internal/domain"
)

type LandingRecoveryBundle struct {
	Intent      domain.LandingIntent         `json:"intent"`
	Candidate   domain.CandidateContextDelta `json:"candidate"`
	ExpectedRef Ref                          `json:"expected_ref"`
}

type LandingTransport interface {
	Landings(context.Context) ([]domain.LandingIntent, error)
	Landing(context.Context, string) (domain.LandingIntent, error)
	LandingRecovery(context.Context, string) (LandingRecoveryBundle, error)
}

var _ LandingTransport = (*HTTP)(nil)

func (h *HTTP) Landings(ctx context.Context) ([]domain.LandingIntent, error) {
	response, err := h.do(ctx, http.MethodGet, h.baseURL+"/landings", nil, "")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, remoteResponseError(response)
	}
	var landings []domain.LandingIntent
	if err := decodeLandingJSON(response.Body, &landings); err != nil {
		return nil, err
	}
	return landings, nil
}

func (h *HTTP) Landing(ctx context.Context, id string) (domain.LandingIntent, error) {
	response, err := h.do(ctx, http.MethodGet, h.landingURL(id), nil, "")
	if err != nil {
		return domain.LandingIntent{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return domain.LandingIntent{}, remoteResponseError(response)
	}
	var landing domain.LandingIntent
	if err := decodeLandingJSON(response.Body, &landing); err != nil {
		return domain.LandingIntent{}, err
	}
	return landing, nil
}

func (h *HTTP) LandingRecovery(ctx context.Context, id string) (LandingRecoveryBundle, error) {
	response, err := h.do(ctx, http.MethodGet, h.landingURL(id)+"/recovery", nil, "")
	if err != nil {
		return LandingRecoveryBundle{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return LandingRecoveryBundle{}, remoteResponseError(response)
	}
	var bundle LandingRecoveryBundle
	if err := decodeLandingJSON(response.Body, &bundle); err != nil {
		return LandingRecoveryBundle{}, err
	}
	return bundle, nil
}

func (h *HTTP) landingURL(id string) string {
	return h.baseURL + "/landings/" + url.PathEscape(id)
}

func decodeLandingJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxCandidatePublicationBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return domain.NewError(domain.CodeValidation, errors.New("decode landing response"))
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.NewError(domain.CodeValidation, errors.New("landing response must contain one JSON value"))
	}
	return nil
}
