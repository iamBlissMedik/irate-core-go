# Payments

How wallet funding through an external provider works, how to test it locally,
and how to plug in a real provider.

## Deposit flow

```mermaid
sequenceDiagram
    autonumber
    participant C as Client (BFF)
    participant API as irate-core-go
    participant DB as PostgreSQL
    participant P as Provider

    C->>API: POST /payments/deposits {amount} + Idempotency-Key
    API->>DB: INSERT deposit PENDING (unique user+key) ON CONFLICT DO NOTHING
    API->>P: InitiateDeposit(reference, amount)   (outside any DB transaction)
    P-->>API: checkout URL
    API->>DB: save checkout_url
    API-->>C: 201 {id, reference, status: PENDING, checkoutUrl}
    C->>P: customer pays on the provider's page
    P->>API: POST /webhooks/payments/{provider} (signed)
    API->>API: verify signature + timestamp
    API->>DB: BEGIN; INSERT webhook_event (unique provider+event_id)
    API->>DB: SELECT deposit … FOR UPDATE
    API->>DB: ledger: debit SETTLEMENT, credit wallet; deposit → SUCCEEDED; audit; COMMIT
    API-->>P: 200 (stop retrying)
```

## Guarantees

| Situation | Outcome |
| --- | --- |
| Client retries "start deposit" with the same key | Same deposit and reference returned (200 + `Idempotent-Replayed`) |
| Same key, different amount | 422 `IDEMPOTENCY_KEY_REUSED` |
| Provider down while starting | 502. The deposit stays PENDING without a URL; a retry with the same key re-attempts the provider call |
| Webhook redelivered (same event id) | No-op (unique `(provider, event_id)`) |
| Two different success events for one deposit | First credits, second is a no-op (status compare-and-set under row lock) |
| Concurrent deliveries | Serialised by `FOR UPDATE` on the deposit; credited once (proven by a 20-way integration test) |
| Failure event after success | Ignored: success is terminal |
| Success after failure (late capture) | Credited: the money really arrived |
| Reported amount/currency ≠ requested | Deposit marked FAILED (`amount_mismatch…`), audit `DEPOSIT_AMOUNT_MISMATCH`, **no credit**. Needs manual review |
| Unknown reference | 200 (retrying will not help); the raw event is stored in `webhook_events` for investigation |
| Bad signature / stale timestamp | 401 `INVALID_SIGNATURE` |
| Success webhook not (yet) confirmed by the provider's verify API (Paystack) | 502, nothing recorded, so the provider redelivers; credited only once verified, using the verified amount |
| Transient error (DB down) | 5xx, so the provider retries later |
| Wallet frozen | Deposit still credits (freeze is "post no debit") |

## Local testing with the sandbox provider

The sandbox signs webhooks exactly like a real provider would: hex HMAC-SHA256
over `"<unix timestamp>.<raw body>"`, sent in `X-Sandbox-Timestamp` and
`X-Sandbox-Signature`, with a 5-minute tolerance. It has no hosted payment
page: the `checkoutUrl` it returns is informational only.

**Easiest:** the simulate endpoint (non-production only) builds a signed event for
your own deposit and runs it through the real webhook path:

```bash
DEP=$(curl -s localhost:8000/api/v1/payments/deposits -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H "Idempotency-Key: $(uuidgen)" -d '{"amount":500000}' | jq -r .data.id)

curl -s localhost:8000/api/v1/payments/sandbox/deposits/$DEP/simulate \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"outcome":"success"}'
```

**Manual webhook** (what the provider would send):

```bash
BODY='{"id":"evt_123","type":"payment.succeeded","data":{"reference":"DEP-…","amount":500000,"currency":"NGN"}}'
TS=$(date +%s)
SIG=$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$SANDBOX_WEBHOOK_SECRET" -hex | awk '{print $NF}')
curl -s localhost:8000/api/v1/webhooks/payments/sandbox -H 'Content-Type: application/json' \
  -H "X-Sandbox-Timestamp: $TS" -H "X-Sandbox-Signature: $SIG" -d "$BODY"
```

## Paystack

`internal/payment/paystack.go` implements both `Provider` and the optional
`Verifier` interface.

| Step | Paystack API | Notes |
| --- | --- | --- |
| Start deposit | `POST /transaction/initialize` `{email, amount, currency, reference, callback_url}` | Amount is sent in kobo unchanged; our `DEP-…` reference is Paystack's reference; returns `authorization_url` as `checkoutUrl` |
| Webhook auth | `X-Paystack-Signature` = hex HMAC-SHA512(secret key, raw body) | Constant-time compare (`hmac.Equal`) |
| Confirm | `GET /transaction/verify/:reference` | Called for every `charge.success` **before** crediting, outside any DB transaction; the verified amount and currency are authoritative |
| Events | `charge.success` (with `data.status = success`) credits | Everything else (transfers, refunds, disputes) is acknowledged and ignored; an abandoned checkout sends nothing, so the deposit stays PENDING |

Why verify? Paystack webhooks carry no timestamp, so a captured, validly
signed webhook could be replayed, and a leaked secret key could be used to
forge one. With verification, money moves only when Paystack itself confirms
the charge. If verification fails or isn't available yet, the API answers 502
and records nothing, so Paystack's automatic redelivery succeeds later.

### Enabling it

```bash
PAYMENTS_PROVIDER=paystack
PAYSTACK_SECRET_KEY=sk_test_...             # sk_live_... in production
PAYSTACK_CALLBACK_URL=https://app.example.com/deposits/complete   # optional
```

Then, in the Paystack dashboard (Settings → API Keys & Webhooks), set the
webhook URL to `https://<host>/api/v1/webhooks/payments/paystack`. Optionally
allowlist Paystack's webhook IPs at your edge.

Startup validation rejects a missing or malformed key and warns when a test key
is used in production. Outside production the sandbox stays registered, so
sandbox deposits (and the simulate endpoint) keep working alongside Paystack.
In production only the active provider is registered.

**Tested** by `internal/payment/paystack_test.go` (request shape, auth,
escaping, error surfacing, signature checks, event mapping) and by
`TestPaystackEndToEnd` in `internal/integration`, which boots a fully wired
app with `PAYMENTS_PROVIDER=paystack` against a Paystack API stub. It covers
initialise → webhook before verification (502, no credit) → verification
says abandoned (502, no credit) → verified success (credited once,
redelivery is a no-op) → verified amount mismatch (flagged, not credited) →
forged signature (401).

Known edge: Paystack rejects re-initialising an existing reference. If our
first `initialize` call reached Paystack but its response was lost, a retry
with the same Idempotency-Key gets a 502 `PAYMENT_PROVIDER_ERROR`; the
customer can start a new deposit (new key).

## Adding another provider (Flutterwave, Stripe…)

1. Implement `payment.Provider` (`Name`, `InitiateDeposit`, `ParseWebhook`) and,
   if the provider has a verify/retrieve endpoint, `payment.Verifier`. Use an
   `http.Client` with a timeout, pass `ctx`, and treat non-2xx as an error.
2. Add its config and a `case` in `newPaymentService` (`internal/app/routes.go`).
3. Point the provider's webhook at `https://<host>/api/v1/webhooks/payments/<name>`.
4. Add unit tests against an `httptest` stub, and an integration test like
   `TestPaystackEndToEnd`.

Settlement, idempotency, auditing and ledger postings are provider-agnostic,
so nothing else changes. For production, also run a daily reconciliation
against the provider's settlement report.

## Roadmap: payouts (withdrawals)

Payouts are asynchronous and can fail after money is reserved, so they need a
**hold**:

1. Request: lock the wallet, check the balance, and post *debit customer → credit
   `PAYOUTS_PENDING`* (a new system account). Create the payout as PENDING with an idempotency key.
2. Call the provider's transfer API outside the transaction.
3. Webhook success: post *debit `PAYOUTS_PENDING` → credit `SETTLEMENT`* and mark it SUCCEEDED.
4. Webhook failure or timeout: post the reversal *debit `PAYOUTS_PENDING` → credit
   customer* and mark it FAILED/REVERSED (the `transaction_status` enum already
   has these values).

The customer can't double-spend held funds, and every state is a balanced
journal, so reconciliation keeps working unchanged.
