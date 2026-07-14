package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

const durablePayloadVersion = 1

type durableEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	Kind          string          `json:"kind"`
	Body          json.RawMessage `json:"body"`
}

func MarshalDurablePayload(kind string, body any) ([]byte, error) {
	if strings.TrimSpace(kind) == "" || body == nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("durable payload kind and body are required"))
	}
	bodyPayload, err := json.Marshal(body)
	if err != nil {
		return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("serialize durable payload body: %w", err))
	}
	payload, err := json.Marshal(durableEnvelope{SchemaVersion: durablePayloadVersion, Kind: kind, Body: bodyPayload})
	if err != nil {
		return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("serialize durable payload envelope: %w", err))
	}
	return payload, nil
}

func UnmarshalDurablePayload(payload []byte, expectedKind string, target any) error {
	if len(payload) == 0 || strings.TrimSpace(expectedKind) == "" || target == nil {
		return domain.NewError(domain.CodeValidation, errors.New("durable payload decode input is incomplete"))
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var envelope durableEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return domain.NewError(domain.CodeValidation, errors.New("durable payload envelope is invalid"))
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.NewError(domain.CodeValidation, errors.New("durable payload envelope must contain one JSON value"))
	}
	if envelope.SchemaVersion != durablePayloadVersion {
		return domain.NewError(domain.CodeIncompatiblePayload, fmt.Errorf("unsupported durable payload schema version %d", envelope.SchemaVersion))
	}
	if envelope.Kind != expectedKind || len(envelope.Body) == 0 {
		return domain.NewError(domain.CodeValidation, errors.New("durable payload kind or body is invalid"))
	}
	bodyDecoder := json.NewDecoder(bytes.NewReader(envelope.Body))
	bodyDecoder.DisallowUnknownFields()
	if err := bodyDecoder.Decode(target); err != nil {
		return domain.NewError(domain.CodeValidation, errors.New("durable payload body is invalid"))
	}
	if err := bodyDecoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.NewError(domain.CodeValidation, errors.New("durable payload body must contain one JSON value"))
	}
	return nil
}
