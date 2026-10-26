// Package kyc implements Know-Your-Customer submission and admin review.
//
// State machine:  (none) -> PENDING -> VERIFIED
//
//	\-> REJECTED -> PENDING (resubmission)
//
// VERIFIED is terminal. Every transition is a compare-and-set on the current
// status, so concurrent requests can never skip or double-apply a step.
package kyc

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/audit"
	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/httpx"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
)

var (
	ErrNotFound        = apperr.NotFound(apperr.CodeKYCNotFound, "No KYC record found.")
	ErrPending         = apperr.Conflict(apperr.CodeKYCPending, "Your KYC is already submitted and pending review.")
	ErrAlreadyVerified = apperr.Conflict(apperr.CodeKYCAlreadyVerified, "Your KYC is already verified and cannot be modified.")
	elevenDigits       = regexp.MustCompile(`^[0-9]{11}$`)
)

const (
	minimumAge = 18
	maximumAge = 120
	dateLayout = "2006-01-02"
)

type SubmitRequest struct {
	FullName    string `json:"fullName" validate:"required,min=2,max=120,printable"`
	DateOfBirth string `json:"dateOfBirth" validate:"required"`
	Address     string `json:"address" validate:"required,min=5,max=200,printable"`
	IDType      string `json:"idType" validate:"required,oneof=NIN BVN PASSPORT DRIVERS_LICENSE VOTERS_CARD"`
	IDNumber    string `json:"idNumber" validate:"required,min=4,max=40,printable"`
	DocumentURL string `json:"documentUrl" validate:"omitempty,max=2048,http_url"`
}

func (r *SubmitRequest) Normalize() {
	r.FullName = strings.Join(strings.Fields(r.FullName), " ") // collapse inner whitespace
	r.DateOfBirth = strings.TrimSpace(r.DateOfBirth)
	r.Address = strings.TrimSpace(r.Address)
	r.IDType = strings.ToUpper(strings.TrimSpace(r.IDType))
	r.IDNumber = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(r.IDNumber), " ", ""))
	r.DocumentURL = strings.TrimSpace(r.DocumentURL)
}

type ReviewRequest struct {
	Action string `json:"action" validate:"required,oneof=APPROVE REJECT"`
	Reason string `json:"reason" validate:"max=500,printable"`
}

func (r *ReviewRequest) Normalize() {
	r.Action = strings.ToUpper(strings.TrimSpace(r.Action))
	r.Reason = strings.TrimSpace(r.Reason)
}

// Record is the KYC representation (kycRecordSchema in @irate/contracts).
type Record struct {
	ID              uuid.UUID  `json:"id"`
	UserID          uuid.UUID  `json:"userId"`
	FullName        string     `json:"fullName"`
	DateOfBirth     string     `json:"dateOfBirth"` // YYYY-MM-DD
	Address         string     `json:"address"`
	IDType          string     `json:"idType"`
	IDNumber        string     `json:"idNumber"`
	DocumentURL     *string    `json:"documentUrl"`
	Status          string     `json:"status"`
	RejectionReason *string    `json:"rejectionReason"`
	ReviewedAt      *time.Time `json:"reviewedAt"`
	VerifiedAt      *time.Time `json:"verifiedAt"`
	SubmittedAt     time.Time  `json:"submittedAt"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

func recordFromRow(k db.KycRecord) Record {
	return Record{
		ID: k.ID, UserID: k.UserID, FullName: k.FullName, DateOfBirth: k.DateOfBirth.Format(dateLayout),
		Address: k.Address, IDType: k.IDType, IDNumber: k.IDNumber, DocumentURL: k.DocumentUrl,
		Status: string(k.Status), RejectionReason: k.RejectionReason, ReviewedAt: k.ReviewedAt,
		VerifiedAt: k.VerifiedAt, SubmittedAt: k.SubmittedAt, CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt,
	}
}

// AdminRecord adds the owning user for the review queue.
type AdminRecord struct {
	Record
	User struct {
		ID    uuid.UUID `json:"id"`
		Email string    `json:"email"`
		Role  string    `json:"role"`
	} `json:"user"`
}

type ListResult struct {
	KYCs       []AdminRecord  `json:"kycs"`
	Pagination httpx.PageMeta `json:"pagination"`
}

type Service struct {
	store *database.Store
	loc   *time.Location
	now   func() time.Time
}

func NewService(store *database.Store, loc *time.Location) *Service {
	return &Service{store: store, loc: loc, now: time.Now}
}

// Submit creates a KYC record, or resubmits one that was REJECTED.
func (s *Service) Submit(ctx context.Context, userID uuid.UUID, req SubmitRequest) (Record, error) {
	dob, err := s.parseDateOfBirth(req.DateOfBirth)
	if err != nil {
		return Record{}, err
	}
	if (req.IDType == "NIN" || req.IDType == "BVN") && !elevenDigits.MatchString(req.IDNumber) {
		return Record{}, apperr.Field("idNumber", "must be exactly 11 digits for "+req.IDType)
	}
	var docURL *string
	if req.DocumentURL != "" {
		docURL = &req.DocumentURL
	}

	var rec db.KycRecord
	err = s.store.InTx(ctx, func(q *db.Queries) error {
		existing, err := q.GetKYCByUserID(ctx, userID)
		switch {
		case database.IsNoRows(err):
			rec, err = q.CreateKYC(ctx, db.CreateKYCParams{
				ID: uuid.Must(uuid.NewV7()), UserID: userID, FullName: req.FullName, DateOfBirth: dob,
				Address: req.Address, IDType: req.IDType, IDNumber: req.IDNumber, DocumentUrl: docURL,
			})
			if database.IsUniqueViolation(err, "kyc_records_user_id_key") {
				return ErrPending // a concurrent submission won the race
			}
		case err != nil:
			return fmt.Errorf("get kyc: %w", err)
		case existing.Status == db.KycStatusPENDING:
			return ErrPending
		case existing.Status == db.KycStatusVERIFIED:
			return ErrAlreadyVerified
		default: // REJECTED
			rec, err = q.ResubmitRejectedKYC(ctx, db.ResubmitRejectedKYCParams{
				UserID: userID, FullName: req.FullName, DateOfBirth: dob, Address: req.Address,
				IDType: req.IDType, IDNumber: req.IDNumber, DocumentUrl: docURL,
			})
			if database.IsNoRows(err) {
				return ErrPending // status changed under us
			}
		}
		if err != nil {
			return fmt.Errorf("save kyc: %w", err)
		}
		// The ID number is PII: it is deliberately not copied into the audit log.
		return audit.Record(ctx, q, audit.Entry{
			ActorID: &userID, Action: audit.ActionKYCSubmitted, ResourceType: "kyc", ResourceID: rec.ID.String(),
			Metadata: map[string]any{"idType": req.IDType},
		})
	})
	if err != nil {
		return Record{}, err
	}
	return recordFromRow(rec), nil
}

// parseDateOfBirth accepts "YYYY-MM-DD" or an RFC 3339 timestamp (the date
// part is used) and enforces 18 <= age <= 120.
func (s *Service) parseDateOfBirth(raw string) (time.Time, error) {
	var dob time.Time
	if d, err := time.Parse(dateLayout, raw); err == nil {
		dob = d
	} else if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		dob = time.Date(ts.Year(), ts.Month(), ts.Day(), 0, 0, 0, 0, time.UTC)
	} else {
		return time.Time{}, apperr.Field("dateOfBirth", "must be a date in YYYY-MM-DD format")
	}

	today := s.now().In(s.loc)
	todayDate := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	switch {
	case !dob.Before(todayDate):
		return time.Time{}, apperr.Field("dateOfBirth", "must be in the past")
	case dob.AddDate(minimumAge, 0, 0).After(todayDate):
		return time.Time{}, apperr.Field("dateOfBirth", fmt.Sprintf("you must be at least %d years old", minimumAge))
	case dob.AddDate(maximumAge, 0, 0).Before(todayDate):
		return time.Time{}, apperr.Field("dateOfBirth", "is not a plausible date of birth")
	}
	return dob, nil
}

// Mine returns the caller's KYC record.
func (s *Service) Mine(ctx context.Context, userID uuid.UUID) (Record, error) {
	k, err := s.store.GetKYCByUserID(ctx, userID)
	if database.IsNoRows(err) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("get kyc: %w", err)
	}
	return recordFromRow(k), nil
}

// List is the admin review queue, optionally filtered by status.
func (s *Service) List(ctx context.Context, status string, p httpx.Page) (ListResult, error) {
	var filter db.NullKycStatus
	if status != "" {
		st := db.KycStatus(strings.ToUpper(status))
		if !st.Valid() {
			return ListResult{}, apperr.Field("status", "must be one of: PENDING, VERIFIED, REJECTED")
		}
		filter = db.NullKycStatus{KycStatus: st, Valid: true}
	}
	rows, err := s.store.ListKYC(ctx, db.ListKYCParams{Status: filter, Lim: p.Lim(), Off: p.Offset()})
	if err != nil {
		return ListResult{}, fmt.Errorf("list kyc: %w", err)
	}
	total, err := s.store.CountKYC(ctx, filter)
	if err != nil {
		return ListResult{}, fmt.Errorf("count kyc: %w", err)
	}
	out := ListResult{KYCs: make([]AdminRecord, 0, len(rows)), Pagination: httpx.NewPageMeta(total, p)}
	for _, r := range rows {
		ar := AdminRecord{Record: recordFromRow(db.KycRecord{
			ID: r.ID, UserID: r.UserID, FullName: r.FullName, DateOfBirth: r.DateOfBirth, Address: r.Address,
			IDType: r.IDType, IDNumber: r.IDNumber, DocumentUrl: r.DocumentUrl, Status: r.Status,
			RejectionReason: r.RejectionReason, ReviewedBy: r.ReviewedBy, ReviewedAt: r.ReviewedAt,
			VerifiedAt: r.VerifiedAt, SubmittedAt: r.SubmittedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})}
		ar.User.ID, ar.User.Email, ar.User.Role = r.UserID, r.UserEmail, string(r.UserRole)
		out.KYCs = append(out.KYCs, ar)
	}
	return out, nil
}

// Review approves or rejects a PENDING submission.
func (s *Service) Review(ctx context.Context, reviewerID, kycID uuid.UUID, req ReviewRequest) (Record, error) {
	status := db.KycStatusREJECTED
	if req.Action == "APPROVE" {
		status = db.KycStatusVERIFIED
	}
	var reason *string
	if status == db.KycStatusREJECTED && req.Reason != "" {
		reason = &req.Reason
	}

	var rec db.KycRecord
	err := s.store.InTx(ctx, func(q *db.Queries) error {
		current, err := q.GetKYCByID(ctx, kycID)
		if database.IsNoRows(err) {
			return apperr.NotFound(apperr.CodeKYCNotFound, "KYC record not found.")
		} else if err != nil {
			return fmt.Errorf("get kyc: %w", err)
		}
		if current.UserID == reviewerID {
			return apperr.Forbidden("You cannot review your own KYC submission.")
		}

		rec, err = q.ReviewPendingKYC(ctx, db.ReviewPendingKYCParams{
			ID: kycID, Status: status, RejectionReason: reason, ReviewedBy: &reviewerID,
		})
		if database.IsNoRows(err) {
			return apperr.Conflict(apperr.CodeKYCAlreadyReviewed,
				fmt.Sprintf("This KYC submission has already been reviewed (status: %s).", current.Status))
		} else if err != nil {
			return fmt.Errorf("review kyc: %w", err)
		}
		return audit.Record(ctx, q, audit.Entry{
			ActorID: &reviewerID, Action: audit.ActionKYCReviewed, ResourceType: "kyc", ResourceID: kycID.String(),
			Metadata: map[string]any{"decision": string(status), "userId": rec.UserID.String(), "reason": derefOr(reason, "")},
		})
	})
	if err != nil {
		return Record{}, err
	}
	return recordFromRow(rec), nil
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}
