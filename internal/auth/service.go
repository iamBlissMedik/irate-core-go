// Package auth implements registration, login, session refresh and logout.
//
// Session model:
//   - Access token: a 15-minute HS256 JWT sent as "Authorization: Bearer".
//     Stateless to verify; revocable through a Redis denylist.
//   - Refresh token: an opaque 256-bit random string in an HttpOnly,
//     SameSite=Strict cookie, stored hashed in Postgres. It is ROTATED on
//     every refresh; presenting an already-rotated token (outside a short
//     grace window for racing tabs) is treated as theft and revokes the whole
//     session family.
package auth

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/iamBlissMedik/irate-core-go/internal/apperr"
	"github.com/iamBlissMedik/irate-core-go/internal/audit"
	"github.com/iamBlissMedik/irate-core-go/internal/config"
	"github.com/iamBlissMedik/irate-core-go/internal/db"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/database"
	"github.com/iamBlissMedik/irate-core-go/internal/platform/metrics"
	"github.com/iamBlissMedik/irate-core-go/internal/reqctx"
	"github.com/iamBlissMedik/irate-core-go/internal/wallet"
)

var (
	ErrEmailTaken = apperr.Conflict(apperr.CodeEmailTaken, "An account with this email already exists.")
	// One message for "no such email" and "wrong password", so the login
	// endpoint cannot be used to discover which emails have accounts.
	ErrInvalidCredentials = apperr.Unauthorized(apperr.CodeInvalidCredentials, "Invalid email or password.")
	ErrRefreshInvalid     = apperr.Unauthorized(apperr.CodeRefreshInvalid, "Your session has expired. Please sign in again.")
)

// User is the public user shape returned by auth endpoints.
type User struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}

func userFromRow(u db.User) User {
	return User{ID: u.ID, Email: u.Email, Role: string(u.Role), CreatedAt: u.CreatedAt}
}

type RegisterRequest struct {
	Email    string `json:"email" validate:"required,email,max=254"`
	Password string `json:"password" validate:"required,password"`
}

func (r *RegisterRequest) Normalize() { r.Email = NormalizeEmail(r.Email) }

type LoginRequest struct {
	Email    string `json:"email" validate:"required,email,max=254"`
	Password string `json:"password" validate:"required,max=128"`
}

func (r *LoginRequest) Normalize() { r.Email = NormalizeEmail(r.Email) }

// NormalizeEmail trims and lower-cases an email (the database enforces it).
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

type RegisterResult struct {
	User          User   `json:"user"`
	AccountNumber string `json:"accountNumber"`
}

// Session is returned by login and refresh. The refresh token travels only in
// the HttpOnly cookie, never in the JSON body.
type Session struct {
	AccessToken      string    `json:"accessToken"`
	TokenType        string    `json:"tokenType"`
	ExpiresIn        int       `json:"expiresIn"` // seconds
	User             User      `json:"user"`
	RefreshToken     string    `json:"-"`
	RefreshExpiresAt time.Time `json:"-"`
}

type Deps struct {
	Store           *database.Store
	Wallets         *wallet.Service
	Tokens          *TokenManager
	Hasher          *Hasher
	Lockout         *Lockout
	Revocations     *Revocations
	Config          config.AuthConfig
	DefaultCurrency string
	Metrics         *metrics.Metrics
	Log             *slog.Logger
}

type Service struct {
	Deps
	now func() time.Time
}

func NewService(d Deps) *Service { return &Service{Deps: d, now: time.Now} }

// Register creates a USER and their default-currency wallet atomically, so a
// new customer can receive money immediately.
func (s *Service) Register(ctx context.Context, req RegisterRequest) (RegisterResult, error) {
	hash, err := s.Hasher.Hash(ctx, req.Password) // outside the tx: never hold a connection while hashing
	if err != nil {
		return RegisterResult{}, fmt.Errorf("hash password: %w", err)
	}

	var res RegisterResult
	err = s.Store.InTx(ctx, func(q *db.Queries) error {
		u, err := q.CreateUser(ctx, db.CreateUserParams{
			ID: uuid.Must(uuid.NewV7()), Email: req.Email, PasswordHash: hash, Role: db.UserRoleUSER,
		})
		if database.IsUniqueViolation(err, "users_email_key") {
			return ErrEmailTaken
		} else if err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		w, err := s.Wallets.Open(ctx, q, u.ID, s.DefaultCurrency)
		if err != nil {
			return err
		}
		res = RegisterResult{User: userFromRow(u), AccountNumber: w.AccountNumber}
		return audit.Record(ctx, q, audit.Entry{
			ActorID: &u.ID, Action: audit.ActionUserRegistered, ResourceType: "user", ResourceID: u.ID.String(),
			Metadata: map[string]any{"accountNumber": w.AccountNumber},
		})
	})
	return res, err
}

// Login verifies credentials and starts a new session family.
func (s *Service) Login(ctx context.Context, req LoginRequest) (Session, error) {
	locked, retryAfter, err := s.Lockout.Locked(ctx, req.Email)
	if err != nil {
		// Fail open: argon2 still makes guessing slow, and the per-IP rate
		// limit still applies. Refusing all logins when Redis blips would
		// turn a cache outage into a full outage.
		s.Log.WarnContext(ctx, "login lockout check unavailable", "err", err)
		s.Metrics.DependencyFailOpen.WithLabelValues("login_lockout").Inc()
	}
	if locked {
		mins := int(math.Ceil(retryAfter.Minutes()))
		return Session{}, apperr.TooManyRequests(apperr.CodeAccountLocked,
			fmt.Sprintf("Too many failed sign-in attempts. Try again in %d minute(s).", max(mins, 1)), retryAfter)
	}

	u, err := s.Store.GetUserByEmail(ctx, req.Email)
	if database.IsNoRows(err) {
		s.Hasher.VerifyDummy(ctx, req.Password)
		s.recordFailure(ctx, req.Email)
		return Session{}, ErrInvalidCredentials
	} else if err != nil {
		return Session{}, fmt.Errorf("get user: %w", err)
	}

	ok, err := s.Hasher.Verify(ctx, req.Password, u.PasswordHash)
	if err != nil {
		return Session{}, fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		s.recordFailure(ctx, req.Email)
		return Session{}, ErrInvalidCredentials
	}

	if err := s.Lockout.Reset(ctx, req.Email); err != nil {
		s.Log.WarnContext(ctx, "reset login lockout", "err", err)
	}
	s.upgradeHash(ctx, u, req.Password)

	session, err := s.issueSession(ctx, s.Store.Queries, u, uuid.Must(uuid.NewV7()))
	if err != nil {
		return Session{}, err
	}
	if err := audit.Record(ctx, s.Store.Queries, audit.Entry{
		ActorID: &u.ID, Action: audit.ActionLoginSucceeded, ResourceType: "user", ResourceID: u.ID.String(),
		Metadata: map[string]any{"userAgent": reqctx.UserAgent(ctx)},
	}); err != nil {
		s.Log.WarnContext(ctx, "audit login", "err", err) // never fail a login over an audit row
	}
	return session, nil
}

func (s *Service) recordFailure(ctx context.Context, email string) {
	if err := s.Lockout.Fail(ctx, email); err != nil {
		s.Log.WarnContext(ctx, "record login failure", "err", err)
		s.Metrics.DependencyFailOpen.WithLabelValues("login_lockout").Inc()
	}
}

// upgradeHash re-hashes with current parameters after a successful login, so
// raising the argon2 cost upgrades existing users transparently.
func (s *Service) upgradeHash(ctx context.Context, u db.User, password string) {
	if !s.Hasher.NeedsRehash(u.PasswordHash) {
		return
	}
	hash, err := s.Hasher.Hash(ctx, password)
	if err == nil {
		err = s.Store.UpdateUserPasswordHash(ctx, db.UpdateUserPasswordHashParams{ID: u.ID, PasswordHash: hash})
	}
	if err != nil {
		s.Log.WarnContext(ctx, "upgrade password hash", "err", err, "user_id", u.ID)
	}
}

func (s *Service) issueSession(ctx context.Context, q *db.Queries, u db.User, familyID uuid.UUID) (Session, error) {
	access, _, err := s.Tokens.Issue(u.ID, u.Email, reqctx.Role(u.Role))
	if err != nil {
		return Session{}, err
	}
	refresh, hash, err := NewRefreshToken()
	if err != nil {
		return Session{}, err
	}
	expiresAt := s.now().Add(s.Config.RefreshTTL)
	if err := q.CreateRefreshToken(ctx, db.CreateRefreshTokenParams{
		ID:        uuid.Must(uuid.NewV7()),
		UserID:    u.ID,
		FamilyID:  familyID,
		TokenHash: hash,
		ExpiresAt: expiresAt,
		UserAgent: optional(reqctx.UserAgent(ctx)),
		IpAddress: optional(reqctx.ClientIP(ctx)),
	}); err != nil {
		return Session{}, fmt.Errorf("store refresh token: %w", err)
	}
	return Session{
		AccessToken:      access,
		TokenType:        "Bearer",
		ExpiresIn:        int(s.Tokens.TTL().Seconds()),
		User:             userFromRow(u),
		RefreshToken:     refresh,
		RefreshExpiresAt: expiresAt,
	}, nil
}

type refreshOutcome int

const (
	refreshOK refreshOutcome = iota
	refreshInvalid
	refreshRaced // presented again within the grace window: reject, keep family
	refreshReuse // presented again later: likely stolen, family revoked
)

// Refresh rotates a refresh token and issues a new access token. The user is
// re-read from the database, so a role change takes effect at the next
// refresh rather than lasting as long as the session.
func (s *Service) Refresh(ctx context.Context, raw string) (Session, error) {
	if raw == "" {
		return Session{}, ErrRefreshInvalid
	}
	hash := HashRefreshToken(raw)

	var (
		session Session
		outcome = refreshInvalid
	)
	// Every branch returns nil so the transaction COMMITS: a family revocation
	// on reuse must persist even though the caller gets an error.
	err := s.Store.InTx(ctx, func(q *db.Queries) error {
		rt, err := q.GetRefreshTokenForUpdate(ctx, hash)
		if database.IsNoRows(err) {
			return nil
		} else if err != nil {
			return fmt.Errorf("get refresh token: %w", err)
		}

		now := s.now()
		switch {
		case rt.RevokedAt != nil, !rt.ExpiresAt.After(now):
			return nil
		case rt.RotatedAt != nil:
			if now.Sub(*rt.RotatedAt) <= s.Config.RefreshReuseGrace {
				outcome = refreshRaced
				return nil
			}
			outcome = refreshReuse
			if _, err := q.RevokeRefreshTokenFamily(ctx, rt.FamilyID); err != nil {
				return fmt.Errorf("revoke token family: %w", err)
			}
			return audit.Record(ctx, q, audit.Entry{
				ActorID: &rt.UserID, Action: audit.ActionRefreshReuse, ResourceType: "session", ResourceID: rt.FamilyID.String(),
			})
		}

		u, err := q.GetUserByID(ctx, rt.UserID)
		if database.IsNoRows(err) {
			return nil
		} else if err != nil {
			return fmt.Errorf("get user: %w", err)
		}
		if err := q.MarkRefreshTokenRotated(ctx, rt.ID); err != nil {
			return fmt.Errorf("rotate refresh token: %w", err)
		}
		if session, err = s.issueSession(ctx, q, u, rt.FamilyID); err != nil {
			return err
		}
		outcome = refreshOK
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	if outcome == refreshReuse {
		s.Log.WarnContext(ctx, "refresh token reuse detected; session family revoked")
	}
	if outcome != refreshOK {
		return Session{}, ErrRefreshInvalid
	}
	return session, nil
}

// Logout ends the current session. It works with either credential: the
// access token (denylisted until expiry) and/or the refresh cookie (its family
// revoked). It accepts an expired access token's absence gracefully, so a user
// whose access token lapsed can still log out and kill their refresh token.
func (s *Service) Logout(ctx context.Context, principal *reqctx.Principal, rawRefresh string) error {
	if rawRefresh != "" {
		rt, err := s.Store.GetRefreshTokenByHash(ctx, HashRefreshToken(rawRefresh))
		switch {
		case err == nil:
			if _, err := s.Store.RevokeRefreshTokenFamily(ctx, rt.FamilyID); err != nil {
				return fmt.Errorf("revoke session: %w", err)
			}
		case !database.IsNoRows(err):
			return fmt.Errorf("get refresh token: %w", err)
		}
	}
	if principal != nil {
		if err := s.Revocations.RevokeToken(ctx, *principal); err != nil {
			// The refresh token is already dead, so the session ends within
			// one access-token lifetime at worst. Log loudly, do not fail.
			s.Log.ErrorContext(ctx, "denylist access token on logout", "err", err)
		}
	}
	return nil
}

// LogoutAll revokes every session of the user on every device.
func (s *Service) LogoutAll(ctx context.Context, p reqctx.Principal) error {
	n, err := s.Store.RevokeUserRefreshTokens(ctx, p.UserID)
	if err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	if err := s.Revocations.RevokeUserTokens(ctx, p.UserID.String(), s.Tokens.TTL()); err != nil {
		// The user explicitly asked to cut off other devices; report failure
		// instead of pretending live access tokens were revoked.
		return apperr.Unavailable("Could not sign out other devices right now. Please try again.").WithCause(err)
	}
	if err := audit.Record(ctx, s.Store.Queries, audit.Entry{
		ActorID: &p.UserID, Action: audit.ActionLogoutAll, ResourceType: "user", ResourceID: p.UserID.String(),
		Metadata: map[string]any{"sessionsRevoked": n},
	}); err != nil {
		s.Log.WarnContext(ctx, "audit logout-all", "err", err)
	}
	return nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
