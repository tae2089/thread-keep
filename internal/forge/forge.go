package forge

import (
	"context"
	"net/http"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

type ForgeAction string

const (
	ForgeActionOpened      ForgeAction = "opened"
	ForgeActionReopened    ForgeAction = "reopened"
	ForgeActionSynchronize ForgeAction = "synchronize"
	ForgeActionClosed      ForgeAction = "closed"
)

type ChangeState string

const (
	ChangeOpen   ChangeState = "open"
	ChangeClosed ChangeState = "closed"
	ChangeMerged ChangeState = "merged"
)

type CheckState string

const (
	CheckPlanning       CheckState = "planning"
	CheckReady          CheckState = "ready"
	CheckReviewRequired CheckState = "review_required"
	CheckBlocked        CheckState = "blocked"
	CheckSuperseded     CheckState = "superseded"
)

type ForgeEvent struct {
	Provider       string           `json:"provider"`
	DeliveryID     string           `json:"delivery_id"`
	Action         ForgeAction      `json:"action"`
	Change         domain.ChangeKey `json:"change"`
	BaseRef        string           `json:"base_ref"`
	BaseSHA        string           `json:"base_sha"`
	HeadRef        string           `json:"head_ref"`
	HeadSHA        string           `json:"head_sha"`
	InstallationID int64            `json:"installation_id"`
	Ignored        bool             `json:"ignored,omitempty"`
}

type Change struct {
	Key      domain.ChangeKey `json:"key"`
	State    ChangeState      `json:"state"`
	BaseRef  string           `json:"base_ref"`
	BaseSHA  string           `json:"base_sha"`
	HeadRef  string           `json:"head_ref"`
	HeadSHA  string           `json:"head_sha"`
	Merged   bool             `json:"merged"`
	MergeSHA string           `json:"merge_sha,omitempty"`
}

type CheckInput struct {
	Change     domain.ChangeKey
	HeadSHA    string
	State      CheckState
	Summary    string
	PlanURL    string
	CheckRunID int64
}

type CheckPublication struct {
	CheckRunID int64
}

type CheckPublisher interface {
	ReconcileCheck(ctx context.Context, input CheckInput) (CheckPublication, error)
}

type CheckoutGrantInput struct {
	Change domain.ChangeKey
}

type CheckoutGrant struct {
	Repository string
	CloneURL   string
	Token      string
	ExpiresAt  time.Time
}

type Forge interface {
	DecodeWebhook(headers http.Header, body []byte) (ForgeEvent, error)
	GetChange(ctx context.Context, key domain.ChangeKey) (Change, error)
	UpsertCheck(ctx context.Context, input CheckInput) error
	CheckoutGrant(ctx context.Context, input CheckoutGrantInput) (CheckoutGrant, error)
}
