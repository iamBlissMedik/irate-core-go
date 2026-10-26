//go:build integration

package integration

import (
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var kycBody = map[string]any{
	"fullName": "Ada Okafor", "dateOfBirth": "1994-05-17", "address": "12 Admiralty Way, Lekki",
	"idType": "NIN", "idNumber": "12345678901", "documentUrl": "https://files.irate.test/ada-nin.png",
}

func review(t *testing.T, admin customer, kycID uuid.UUID, action string) response {
	return do(t, req{method: http.MethodPatch, path: "/admin/kyc/" + kycID.String() + "/review", token: admin.Token,
		body: map[string]string{"action": action, "reason": "Document unreadable"}})
}

func TestKYCLifecycle(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada := register(t, "ada@irate.test")

	assert.Equal(t, http.StatusNotFound, get(t, ada.Token, "/kyc/status").Status, "no record yet")

	res := post(t, ada.Token, "/kyc/submit", kycBody)
	require.Equal(t, http.StatusCreated, res.Status, string(res.Raw))
	var rec struct {
		ID     uuid.UUID
		Status string
	}
	res.decode(t, &rec)
	assert.Equal(t, "PENDING", rec.Status)

	assert.Equal(t, "KYC_PENDING", post(t, ada.Token, "/kyc/submit", kycBody).Env.Code, "cannot resubmit while pending")

	var queue struct {
		KYCs []struct {
			ID   uuid.UUID
			User struct{ Email string }
		} `json:"kycs"`
	}
	get(t, admin.Token, "/admin/kyc?status=PENDING").decode(t, &queue)
	require.Len(t, queue.KYCs, 1)
	assert.Equal(t, "ada@irate.test", queue.KYCs[0].User.Email)
	assert.Equal(t, http.StatusBadRequest, get(t, admin.Token, "/admin/kyc?status=MAYBE").Status)

	res = review(t, admin, rec.ID, "REJECT")
	require.Equal(t, http.StatusOK, res.Status)
	assert.Equal(t, "KYC rejected", res.Env.Message)

	res = post(t, ada.Token, "/kyc/submit", kycBody)
	require.Equal(t, http.StatusCreated, res.Status, "resubmission after rejection")
	res.decode(t, &rec)
	assert.Equal(t, "PENDING", rec.Status)

	require.Equal(t, http.StatusOK, review(t, admin, rec.ID, "APPROVE").Status)
	assert.Equal(t, "KYC_ALREADY_REVIEWED", review(t, admin, rec.ID, "REJECT").Env.Code, "decisions are final")
	assert.Equal(t, "KYC_ALREADY_VERIFIED", post(t, ada.Token, "/kyc/submit", kycBody).Env.Code)

	var me struct{ KYCStatus string }
	get(t, ada.Token, "/users/me").decode(t, &me)
	assert.Equal(t, "VERIFIED", me.KYCStatus)
}

func TestKYCValidation(t *testing.T) {
	reset(t)
	ada := register(t, "ada@irate.test")
	bad := func(field string, value any) {
		t.Helper()
		body := map[string]any{}
		for k, v := range kycBody {
			body[k] = v
		}
		body[field] = value
		res := post(t, ada.Token, "/kyc/submit", body)
		require.Equal(t, http.StatusBadRequest, res.Status, "%s=%v: %s", field, value, res.Raw)
		assert.Equal(t, field, res.Env.Errors[0].Field)
	}
	bad("dateOfBirth", "2015-01-01") // under 18
	bad("dateOfBirth", "yesterday")
	bad("idType", "LIBRARY_CARD")
	bad("idNumber", "123") // NIN must be 11 digits
	bad("documentUrl", "javascript:alert(1)")
	bad("fullName", "Ada\aOkafor") // control character (newlines are normalised to spaces instead)
}

// Two admins approving/rejecting the same submission at the same moment:
// exactly one decision wins.
func TestConcurrentKYCReviews(t *testing.T) {
	reset(t)
	admin1, admin2 := createAdmin(t, "admin1@irate.test"), createAdmin(t, "admin2@irate.test")
	ada := register(t, "ada@irate.test")
	var rec struct{ ID uuid.UUID }
	post(t, ada.Token, "/kyc/submit", kycBody).decode(t, &rec)

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i, a := range []customer{admin1, admin2} {
		wg.Go(func() { statuses[i] = review(t, a, rec.ID, []string{"APPROVE", "REJECT"}[i]).Status })
	}
	wg.Wait()
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, statuses)
}

func TestAdminCannotReviewOwnKYC(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	res := post(t, admin.Token, "/kyc/submit", kycBody)
	require.Equal(t, http.StatusCreated, res.Status)
	var rec struct{ ID uuid.UUID }
	res.decode(t, &rec)
	assert.Equal(t, http.StatusForbidden, review(t, admin, rec.ID, "APPROVE").Status)
}

func TestAdminCredit(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada := register(t, "ada@irate.test")

	key := newKey()
	body := map[string]any{"amount": 500_000, "reason": "Goodwill: failed card refund"}
	res := post(t, admin.Token, "/admin/credit/"+ada.WalletID.String(), body, "Idempotency-Key", key)
	require.Equal(t, http.StatusOK, res.Status, string(res.Raw))
	var cr struct{ Balance, Reference string }
	res.decode(t, &cr)
	assert.Equal(t, "500000", cr.Balance)
	assert.Regexp(t, `^ADJ-`, cr.Reference)

	replay := post(t, admin.Token, "/admin/credit/"+ada.WalletID.String(), body, "Idempotency-Key", key)
	assert.Equal(t, "true", replay.Header.Get("Idempotent-Replayed"))
	assert.EqualValues(t, 500_000, balance(t, ada), "a double-clicked credit pays out once")

	var stmt struct {
		Transactions []struct {
			Kind, Description string
			Counterparty      *struct{}
		} `json:"transactions"`
	}
	get(t, ada.Token, "/transactions/me").decode(t, &stmt)
	require.Len(t, stmt.Transactions, 1)
	assert.Equal(t, "ADMIN_CREDIT", stmt.Transactions[0].Kind)
	assert.Equal(t, "Goodwill: failed card refund", stmt.Transactions[0].Description)
	assert.Nil(t, stmt.Transactions[0].Counterparty, "system accounts are never exposed as counterparties")

	t.Run("cannot credit own wallet", func(t *testing.T) {
		require.Equal(t, http.StatusCreated, post(t, admin.Token, "/wallets", nil).Status, "admins can open a wallet")
		var ws struct {
			Wallets []struct{ ID uuid.UUID } `json:"wallets"`
		}
		get(t, admin.Token, "/wallets").decode(t, &ws)
		res := post(t, admin.Token, "/admin/credit/"+ws.Wallets[0].ID.String(), body, "Idempotency-Key", newKey())
		assert.Equal(t, http.StatusForbidden, res.Status)
	})
	t.Run("unknown wallet", func(t *testing.T) {
		res := post(t, admin.Token, "/admin/credit/"+uuid.NewString(), body, "Idempotency-Key", newKey())
		assert.Equal(t, http.StatusNotFound, res.Status)
	})
	t.Run("reason required", func(t *testing.T) {
		res := post(t, admin.Token, "/admin/credit/"+ada.WalletID.String(), map[string]any{"amount": 1, "reason": "  "}, "Idempotency-Key", newKey())
		assert.Equal(t, http.StatusBadRequest, res.Status)
	})
	assertLedgerHealthy(t, admin)
}

func TestAdminDashboardAndDirectory(t *testing.T) {
	reset(t)
	admin := createAdmin(t, "admin@irate.test")
	ada, bola := register(t, "ada@irate.test"), register(t, "bola@irate.test")
	verifyKYC(t, admin, ada)
	fund(t, admin, ada, 1_000_000)
	require.Equal(t, http.StatusOK, transfer(t, ada, bola.Account, 200_000, newKey()).Status)

	var ov struct {
		UsersStats        struct{ Value int }
		WalletsBalance    struct{ Value string }
		TransactionStats  struct{ Value int }
		TotalCashflow     struct{ Value, Credit, Debit string }
		TransactionVolume struct{ Value string }
	}
	get(t, admin.Token, "/admin/dashboard/overview").decode(t, &ov)
	assert.Equal(t, 2, ov.UsersStats.Value, "admins are not counted as users")
	assert.Equal(t, "1000000", ov.WalletsBalance.Value, "system accounts excluded")
	assert.Equal(t, 2, ov.TransactionStats.Value, "one credit + one transfer (not 3 rows)")
	assert.Equal(t, "1000000", ov.TotalCashflow.Value, "P2P transfers net out of cashflow")
	assert.Equal(t, "1200000", ov.TransactionVolume.Value, "each transfer counted once")

	var dir struct {
		Users []struct {
			Email   string
			KYC     *struct{ Status string } `json:"kyc"`
			Wallets []struct{ Balance string }
		} `json:"users"`
		Pagination struct{ Total int }
	}
	get(t, admin.Token, "/admin/users?search=ADA").decode(t, &dir)
	require.Len(t, dir.Users, 1, "case-insensitive search")
	assert.Equal(t, "VERIFIED", dir.Users[0].KYC.Status)
	assert.Equal(t, "800000", dir.Users[0].Wallets[0].Balance)

	get(t, admin.Token, "/admin/users?search=%25").decode(t, &dir)
	assert.Empty(t, dir.Users, "LIKE wildcards in search input are escaped")

	get(t, admin.Token, "/admin/users?limit=1&page=2").decode(t, &dir)
	assert.Len(t, dir.Users, 1)
	assert.Equal(t, 3, dir.Pagination.Total)
}
