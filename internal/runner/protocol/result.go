package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
)

const (
	ResultEnvelopeVersion  = 1
	MaxResultEnvelopeBytes = 16 << 20
)

type ResultEnvelope struct {
	Version  int                    `json:"version"`
	Evidence planner.SourceEvidence `json:"evidence"`
	Code     domain.ErrorCode       `json:"code,omitempty"`
	Message  string                 `json:"message,omitempty"`
}

func EncodeResult(envelope ResultEnvelope) ([]byte, error) {
	if envelope.Version == 0 {
		envelope.Version = ResultEnvelopeVersion
	}
	if envelope.Version != ResultEnvelopeVersion {
		return nil, domain.NewError(domain.CodeIncompatiblePayload, errors.New("runner result envelope version is unsupported"))
	}
	contents, err := json.Marshal(envelope)
	if err != nil || len(contents) > MaxResultEnvelopeBytes {
		return nil, domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner result envelope exceeds the limit"))
	}
	return contents, nil
}

func DecodeResult(contents []byte) (ResultEnvelope, error) {
	if len(contents) == 0 || len(contents) > MaxResultEnvelopeBytes {
		return ResultEnvelope{}, domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner result envelope is empty or exceeds the limit"))
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	var envelope ResultEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return ResultEnvelope{}, domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner result envelope is invalid"))
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ResultEnvelope{}, domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner result envelope contains multiple values"))
	}
	if envelope.Version != ResultEnvelopeVersion {
		return ResultEnvelope{}, domain.NewError(domain.CodeIncompatiblePayload, errors.New("runner result envelope version is unsupported"))
	}
	return envelope, nil
}
