//go:build integration

package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func refresh(t *testing.T, cookie string) response {
	return do(t, req{method: http.MethodPost, path: "/auth/refresh", headers: map[string]string{"Cookie": "refreshToken=" + cookie}})
}

func TestRegistration(t *testing.T) {
	reset(t)
	res := post(t, "", "/auth/register", map[string]string{"email": "  Ada@IRate.Test ", "password": testPassword})
	require.Equal(t, http.StatusCreated, res.Status, string(res.Raw))
	var reg struct {
		User          struct{ Email, Role string } `json:"user"`
		AccountNumber string                       `json:"accountNumber"`
	}
	res.decode(t, &reg)
	assert.Equal(t, "ada@irate.test", reg.User.Email, "emails are normalised")
	assert.Equal(t, "USER", reg.User.Role, "self sign-up can never create an admin")
	assert.Regexp(t, `^[1-9][0-9]{9}$`, reg.AccountNumber)

	dup := post(t, "", "/auth/register", map[string]string{"email": "ADA@irate.test", "password": testPassword})
	assert.Equal(t, http.StatusConflict, dup.Status)
	assert.Equal(t, "EMAIL_TAKEN", dup.Env.Code)

	weak := post(t, "", "/auth/register", map[string]string{"email": "bola@irate.test", "password": "password"})
	assert.Equal(t, http.StatusBadRequest, weak.Status)
	assert.Equal(t, "password", weak.Env.Errors[0].Field)

	sneaky := post(t, "", "/auth/register", map[string]any{"email": "eve@irate.test", "password": testPassword, "role": "ADMIN"})
	assert.Equal(t, http.StatusBadRequest, sneaky.Status, "unknown fields such as role are rejected")
}

func TestLoginDoesNotRevealWhichEmailsExist(t *testing.T) {
	reset(t)
	register(t, "ada@irate.test")

	wrongPassword := post(t, "", "/auth/login", map[string]string{"email": "ada@irate.test", "password": "Wrong123!"})
	noSuchUser := post(t, "", "/auth/login", map[string]string{"email": "nobody@irate.test", "password": "Wrong123!"})
	assert.Equal(t, http.StatusUnauthorized, wrongPassword.Status)
	assert.Equal(t, wrongPassword.Status, noSuchUser.Status)
	assert.Equal(t, wrongPassword.Env.Message, noSuchUser.Env.Message)
	assert.Equal(t, "INVALID_CREDENTIALS", noSuchUser.Env.Code)
}

func TestLoginLockout(t *testing.T) {
	reset(t)
	register(t, "ada@irate.test")
	for range 5 {
		res := post(t, "", "/auth/login", map[string]string{"email": "ada@irate.test", "password": "Wrong123!"})
		require.Equal(t, http.StatusUnauthorized, res.Status)
	}
	res := post(t, "", "/auth/login", map[string]string{"email": "ada@irate.test", "password": testPassword})
	assert.Equal(t, http.StatusTooManyRequests, res.Status, "even the right password is refused while locked")
	assert.Equal(t, "ACCOUNT_LOCKED", res.Env.Code)
	assert.NotEmpty(t, res.Header.Get("Retry-After"))
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	reset(t)
	register(t, "ada@irate.test")
	s := login(t, "ada@irate.test", testPassword)
	require.NotEmpty(t, s.RefreshCookie)

	// 1. A refresh rotates the token.
	r1 := refresh(t, s.RefreshCookie)
	require.Equal(t, http.StatusOK, r1.Status, string(r1.Raw))
	rotated := refreshCookie(r1)
	require.NotEmpty(t, rotated)
	assert.NotEqual(t, s.RefreshCookie, rotated)
	var sess struct{ AccessToken string }
	r1.decode(t, &sess)
	assert.Equal(t, http.StatusOK, get(t, sess.AccessToken, "/users/me").Status)

	// 2. Re-presenting the old token within the grace window (two tabs
	//    racing) is refused but does NOT kill the session.
	raced := refresh(t, s.RefreshCookie)
	assert.Equal(t, http.StatusUnauthorized, raced.Status)
	r2 := refresh(t, rotated)
	require.Equal(t, http.StatusOK, r2.Status, "the legitimate session survives a race")
	latest := refreshCookie(r2)

	// 3. Re-presenting an old token after the grace window looks like theft:
	//    the whole family is revoked, including the newest token.
	time.Sleep(1100 * time.Millisecond)
	stolen := refresh(t, s.RefreshCookie)
	assert.Equal(t, http.StatusUnauthorized, stolen.Status)
	assert.Equal(t, http.StatusUnauthorized, refresh(t, latest).Status, "family revoked on reuse")

	// Other sessions (other devices) are unaffected.
	other := login(t, "ada@irate.test", testPassword)
	assert.Equal(t, http.StatusOK, refresh(t, other.RefreshCookie).Status)
}

func TestLogout(t *testing.T) {
	reset(t)
	register(t, "ada@irate.test")
	s := login(t, "ada@irate.test", testPassword)

	res := do(t, req{method: http.MethodPost, path: "/auth/logout", token: s.Token, headers: map[string]string{"Cookie": "refreshToken=" + s.RefreshCookie}})
	require.Equal(t, http.StatusOK, res.Status)
	assert.Contains(t, res.Header.Get("Set-Cookie"), "Max-Age=0", "cookie cleared")

	assert.Equal(t, http.StatusUnauthorized, get(t, s.Token, "/users/me").Status, "access token revoked")
	assert.Equal(t, http.StatusUnauthorized, refresh(t, s.RefreshCookie).Status, "refresh token revoked")

	// Logout works with only the cookie (e.g. the access token already expired).
	s2 := login(t, "ada@irate.test", testPassword)
	res = do(t, req{method: http.MethodPost, path: "/auth/logout", headers: map[string]string{"Cookie": "refreshToken=" + s2.RefreshCookie}})
	assert.Equal(t, http.StatusOK, res.Status)
	assert.Equal(t, http.StatusUnauthorized, refresh(t, s2.RefreshCookie).Status)
}

func TestLogoutAllSessions(t *testing.T) {
	reset(t)
	register(t, "ada@irate.test")
	phone, laptop := login(t, "ada@irate.test", testPassword), login(t, "ada@irate.test", testPassword)

	res := post(t, phone.Token, "/auth/logout-all", nil)
	require.Equal(t, http.StatusOK, res.Status, string(res.Raw))
	for name, s := range map[string]session{"phone": phone, "laptop": laptop} {
		assert.Equal(t, http.StatusUnauthorized, get(t, s.Token, "/users/me").Status, name)
		assert.Equal(t, http.StatusUnauthorized, refresh(t, s.RefreshCookie).Status, name)
	}
	fresh := login(t, "ada@irate.test", testPassword)
	assert.Equal(t, http.StatusOK, get(t, fresh.Token, "/users/me").Status, "logging straight back in works")
}

func TestAuthorizationBoundaries(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada, bola := register(t, "ada@irate.test"), register(t, "bola@irate.test")

	assert.Equal(t, http.StatusUnauthorized, get(t, "", "/users/me").Status)
	assert.Equal(t, http.StatusForbidden, get(t, ada.Token, "/admin/dashboard/overview").Status)
	assert.Equal(t, http.StatusForbidden, get(t, ada.Token, "/users/"+bola.UserID.String()).Status, "no IDOR on profiles")
	assert.Equal(t, http.StatusOK, get(t, admin.Token, "/users/"+bola.UserID.String()).Status, "admins may read any profile")
	assert.Equal(t, http.StatusNotFound, get(t, ada.Token, "/wallets/"+bola.WalletID.String()+"/balance").Status,
		"another user's wallet looks exactly like a missing one")
	assert.Equal(t, http.StatusNotFound, get(t, ada.Token, "/transactions/"+bola.WalletID.String()+"/transactions").Status)
	assert.Equal(t, http.StatusBadRequest, get(t, ada.Token, "/wallets/not-a-uuid/balance").Status)

	var resolved struct{ AccountName string }
	get(t, ada.Token, "/transactions/resolve/"+bola.Account).decode(t, &resolved)
	assert.Equal(t, "Bola", resolved.AccountName)
	assert.NotContains(t, string(get(t, ada.Token, "/transactions/resolve/"+bola.Account).Raw), "@", "name enquiry never leaks the email")
}
