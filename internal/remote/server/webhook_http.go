package server

import (
	"errors"
	"io"
	"net/http"

	"github.com/tae2089/thread-keep/internal/domain"
)

const GitHubWebhookPath = "/v1/providers/github/webhooks"

type WebhookHandler struct {
	ingress *WebhookIngress
}

func NewWebhookHandler(ingress *WebhookIngress) (http.Handler, error) {
	if ingress == nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("webhook handler requires an ingress"))
	}
	return &WebhookHandler{ingress: ingress}, nil
}

func (h *WebhookHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.EscapedPath() != GitHubWebhookPath {
		writeError(writer, &httpError{status: http.StatusNotFound, code: domain.CodeValidation, message: "unknown route"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, maxCoordinatorRequestBytes))
	if err != nil {
		writeError(writer, &httpError{status: http.StatusRequestEntityTooLarge, code: domain.CodeValidation, message: "webhook payload exceeds the size limit or was interrupted"})
		return
	}
	result, err := h.ingress.Intake(request.Context(), request.Header, body)
	if err != nil {
		writeError(writer, err)
		return
	}
	if err := writeJSON(writer, http.StatusAccepted, result); err != nil {
		writeError(writer, err)
	}
}
