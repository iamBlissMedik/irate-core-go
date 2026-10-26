// Package audit records security- and money-relevant actions.
//
// Record takes the transaction-bound *db.Queries, so callers write the audit
// row inside the same database transaction as the action itself: if the action
// rolls back, so does its audit entry, and vice versa. That is stronger than
// the original "best-effort log after the fact" approach.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
)

// Actions. Keep these stable: they are queried by compliance tooling.
const (
	ActionUserRegistered    = "USER_REGISTERED"
	ActionLoginSucceeded    = "LOGIN_SUCCEEDED"
	ActionLogoutAll         = "LOGOUT_ALL_SESSIONS"
	ActionRefreshReuse      = "REFRESH_TOKEN_REUSE_DETECTED"
	ActionWalletCreated     = "WALLET_CREATED"
	ActionWalletStatus      = "WALLET_STATUS_CHANGED"
	ActionTransfer          = "WALLET_TRANSFER"
	ActionAdminCredit       = "ADMIN_CREDIT"
	ActionKYCSubmitted      = "KYC_SUBMITTED"
	ActionKYCReviewed       = "KYC_REVIEWED"
	ActionDepositInitiated  = "DEPOSIT_INITIATED"
	ActionDepositSucceeded  = "DEPOSIT_SUCCEEDED"
	ActionDepositFailed     = "DEPOSIT_FAILED"
	ActionDepositAmountFlag = "DEPOSIT_AMOUNT_MISMATCH"
)

type Entry struct {
	ActorID      *uuid.UUID // nil for system actors (e.g. webhooks)
	Action       string
	ResourceType string
	ResourceID   string
	Metadata     map[string]any
}

// Record writes e, enriched with the request id and client IP from ctx.
func Record(ctx context.Context, q *db.Queries, e Entry) error {
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal audit metadata: %w", err)
	}
	return q.CreateAuditLog(ctx, db.CreateAuditLogParams{
		ActorID:      e.ActorID,
		Action:       e.Action,
		ResourceType: nonEmpty(e.ResourceType),
		ResourceID:   nonEmpty(e.ResourceID),
		Metadata:     raw,
		IpAddress:    nonEmpty(reqctx.ClientIP(ctx)),
		RequestID:    nonEmpty(reqctx.RequestID(ctx)),
	})
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
