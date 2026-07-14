package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/forge"
	"github.com/zeebo/blake3"
)

const processWebhookPriority = 300

type WebhookVerifier interface {
	DecodeWebhook(headers http.Header, body []byte) (forge.ForgeEvent, error)
}

type WebhookIngressConfig struct {
	Refs         *GormRefStore
	Verifier     WebhookVerifier
	Repositories []CoordinatorRepository
	Now          func() time.Time
}

type WebhookIngress struct {
	refs    *GormRefStore
	verify  WebhookVerifier
	byForge map[string]CoordinatorRepository
	now     func() time.Time
}

type processWebhookJobPayload struct {
	Provider   string `json:"provider"`
	DeliveryID string `json:"delivery_id"`
}

func NewWebhookIngress(config WebhookIngressConfig) (*WebhookIngress, error) {
	if config.Refs == nil || config.Verifier == nil || len(config.Repositories) == 0 {
		return nil, domain.NewError(domain.CodeValidation, errors.New("webhook ingress dependencies are incomplete"))
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	ingress := &WebhookIngress{refs: config.Refs, verify: config.Verifier, byForge: make(map[string]CoordinatorRepository, len(config.Repositories)), now: config.Now}
	for _, repository := range config.Repositories {
		if repository.ForgeRepository == "" || repository.InstallationID < 1 || repository.TargetRef == "" {
			return nil, domain.NewError(domain.CodeValidation, errors.New("webhook ingress repository binding is invalid"))
		}
		if _, exists := ingress.byForge[repository.ForgeRepository]; exists {
			return nil, domain.NewError(domain.CodeValidation, errors.New("webhook ingress repository binding is duplicated"))
		}
		ingress.byForge[repository.ForgeRepository] = repository
	}
	return ingress, nil
}

func (i *WebhookIngress) Intake(ctx context.Context, headers http.Header, body []byte) (WebhookIntakeResult, error) {
	event, err := i.verify.DecodeWebhook(headers, body)
	if err != nil {
		return WebhookIntakeResult{}, err
	}
	digest := sha256.Sum256(body)
	delivery := WebhookDelivery{Provider: event.Provider, DeliveryID: event.DeliveryID, PayloadHash: fmt.Sprintf("%x", digest[:]), ReceivedAt: i.now().UTC()}
	repository, bound := i.byForge[event.Change.Repository]
	if event.Ignored || !bound || event.Change.Provider != "github" || event.InstallationID != repository.InstallationID || event.BaseRef != strings.TrimPrefix(repository.TargetRef, "refs/contexts/") {
		accepted, err := i.refs.AcceptDelivery(ctx, delivery)
		if err != nil {
			return WebhookIntakeResult{}, err
		}
		return WebhookIntakeResult{Accepted: true, Duplicate: !accepted, Ignored: accepted}, nil
	}
	eventPayload, err := MarshalDurablePayload("webhook_event", event)
	if err != nil {
		return WebhookIntakeResult{}, err
	}
	jobPayload, err := MarshalDurablePayload(processWebhookJobKind, processWebhookJobPayload{Provider: event.Provider, DeliveryID: event.DeliveryID})
	if err != nil {
		return WebhookIntakeResult{}, err
	}
	dedupeKey := "webhook:" + event.Provider + ":" + event.DeliveryID
	jobDigest := blake3.Sum256([]byte(dedupeKey))
	job := CoordinatorJob{ID: fmt.Sprintf("%x", jobDigest[:]), DedupeKey: dedupeKey, Kind: processWebhookJobKind, Priority: processWebhookPriority, Payload: jobPayload, State: CoordinatorJobPending, MaxAttempts: 5, NextAttemptAt: i.now().UTC()}
	accepted, err := i.refs.AcceptWebhook(ctx, WebhookAccept{Delivery: delivery, EventPayload: eventPayload, Job: job})
	if err != nil {
		return WebhookIntakeResult{}, err
	}
	return WebhookIntakeResult{Accepted: true, Duplicate: !accepted}, nil
}

func decodeWebhookEvent(payload []byte) (forge.ForgeEvent, error) {
	var event forge.ForgeEvent
	if err := UnmarshalDurablePayload(payload, "webhook_event", &event); err != nil {
		return forge.ForgeEvent{}, err
	}
	return event, nil
}
