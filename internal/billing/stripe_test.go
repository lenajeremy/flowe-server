package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"workflow-ai/server/internal/database/models"
)

// Webhook verification is the entire security boundary of billing: without it,
// anyone who learns the endpoint URL can grant themselves any plan. These tests
// cover the ways a hand-rolled HMAC check goes wrong.

const testSecret = "whsec_test_secret_do_not_use"

// signPayload produces a valid Stripe-Signature header for a body.
func signPayload(t *testing.T, body string, at time.Time) string {
	t.Helper()
	ts := fmt.Sprintf("%d", at.Unix())
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(ts + "." + body))
	return fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func withSecret(t *testing.T) {
	t.Helper()
	t.Setenv("STRIPE_WEBHOOK_SECRET", testSecret)
}

func TestValidSignatureIsAccepted(t *testing.T) {
	withSecret(t)
	body := `{"id":"evt_1","type":"invoice.paid"}`
	if err := VerifyWebhook([]byte(body), signPayload(t, body, time.Now())); err != nil {
		t.Fatalf("a correctly signed webhook was rejected: %v", err)
	}
}

func TestTamperedBodyIsRejected(t *testing.T) {
	withSecret(t)
	body := `{"id":"evt_1","type":"customer.subscription.updated"}`
	sig := signPayload(t, body, time.Now())

	// The attack this stops: replay a real event with the plan swapped.
	tampered := strings.Replace(body, "evt_1", "evt_2", 1)
	if err := VerifyWebhook([]byte(tampered), sig); err == nil {
		t.Fatal("a modified body passed verification")
	}
}

func TestWrongSecretIsRejected(t *testing.T) {
	body := `{"id":"evt_1"}`
	sig := signPayload(t, body, time.Now())
	t.Setenv("STRIPE_WEBHOOK_SECRET", "whsec_a_different_secret")
	if err := VerifyWebhook([]byte(body), sig); err == nil {
		t.Fatal("a signature from another secret passed verification")
	}
}

func TestOldTimestampIsRejected(t *testing.T) {
	withSecret(t)
	body := `{"id":"evt_1"}`
	// A captured request replayed later. The signature is genuine, which is exactly
	// why the timestamp has to be checked separately.
	old := signPayload(t, body, time.Now().Add(-30*time.Minute))
	if err := VerifyWebhook([]byte(body), old); err == nil {
		t.Fatal("a replayed webhook from 30 minutes ago was accepted")
	}
	// And a timestamp from the future, which would otherwise extend the replay
	// window indefinitely.
	future := signPayload(t, body, time.Now().Add(30*time.Minute))
	if err := VerifyWebhook([]byte(body), future); err == nil {
		t.Fatal("a webhook timestamped in the future was accepted")
	}
}

func TestSignatureIsCheckedOverTheTimestampedPayloadNotTheBodyAlone(t *testing.T) {
	withSecret(t)
	body := `{"id":"evt_1"}`
	// HMAC of the body WITHOUT the "{timestamp}." prefix. A plausible mistake, and
	// one that would let any timestamp be paired with a signature computed once.
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	wrong := fmt.Sprintf("t=%d,v1=%s", time.Now().Unix(), hex.EncodeToString(mac.Sum(nil)))
	if err := VerifyWebhook([]byte(body), wrong); err == nil {
		t.Fatal("a signature over the body alone was accepted")
	}
}

func TestMalformedHeadersAreRejected(t *testing.T) {
	withSecret(t)
	body := `{"id":"evt_1"}`
	for _, sig := range []string{
		"",
		"garbage",
		"v1=deadbeef",                          // no timestamp
		fmt.Sprintf("t=%d", time.Now().Unix()), // no signature
		"t=notanumber,v1=deadbeef",
	} {
		if err := VerifyWebhook([]byte(body), sig); err == nil {
			t.Fatalf("malformed header %q was accepted", sig)
		}
	}
}

func TestAnyOfSeveralSignaturesMayMatchDuringRotation(t *testing.T) {
	withSecret(t)
	body := `{"id":"evt_1"}`
	valid := signPayload(t, body, time.Now())
	// Stripe sends multiple v1 signatures while a secret is being rotated. Only
	// accepting the first would break every webhook for the rotation window.
	combined := valid + ",v1=" + strings.Repeat("00", 32)
	if err := VerifyWebhook([]byte(body), combined); err != nil {
		t.Fatalf("a valid signature alongside a stale one was rejected: %v", err)
	}
}

func TestMissingSecretRefusesRatherThanAccepting(t *testing.T) {
	t.Setenv("STRIPE_WEBHOOK_SECRET", "")
	body := `{"id":"evt_1"}`
	// The dangerous failure mode: no secret configured, so verification is skipped
	// and every unsigned request is trusted.
	if err := VerifyWebhook([]byte(body), signPayload(t, body, time.Now())); err == nil {
		t.Fatal("verification passed with no configured secret")
	}
}

// ── Plan resolution from Stripe objects ──────────────────────────

func TestPlanFromSubscriptionPrefersThePriceOverStaleMetadata(t *testing.T) {
	t.Setenv("STRIPE_PRICE_PRO", "price_pro_123")
	t.Setenv("STRIPE_PRICE_TEAM", "price_team_456")

	// A plan switched through the Customer Portal arrives with a new price but the
	// metadata from the ORIGINAL checkout. Trusting metadata would keep the customer
	// on the plan they left.
	sub := newSub("price_team_456", map[string]any{"plan": "pro"})
	if got := planFromSubscription(sub); got != models.PlanTeam {
		t.Fatalf("plan = %q, want team (from the price, not the stale metadata)", got)
	}
}

func TestPlanFromSubscriptionFallsBackToMetadata(t *testing.T) {
	t.Setenv("STRIPE_PRICE_PRO", "price_pro_123")
	// A price we do not recognise — a legacy or hand-made one. Metadata is the only
	// signal left.
	sub := newSub("price_legacy_999", map[string]any{"plan": "pro"})
	if got := planFromSubscription(sub); got != models.PlanPro {
		t.Fatalf("plan = %q, want pro from metadata", got)
	}
}

func TestUnknownPlanMetadataIsNotHonoured(t *testing.T) {
	sub := newSub("price_unknown", map[string]any{"plan": "unlimited-everything"})
	if got := planFromSubscription(sub); got != "" {
		t.Fatalf("plan = %q, want empty for an unrecognised plan name", got)
	}
}

func TestEmptyPriceEnvDoesNotMatchAnEmptyPriceID(t *testing.T) {
	// With STRIPE_PRICE_PRO unset, a subscription whose price id is somehow empty
	// must not resolve to Pro by both being "".
	t.Setenv("STRIPE_PRICE_PRO", "")
	t.Setenv("STRIPE_PRICE_TEAM", "")
	sub := newSub("", nil)
	if got := planFromSubscription(sub); got != "" {
		t.Fatalf("plan = %q, want empty — an unset price env matched an empty price", got)
	}
}

func TestBusinessAndFreeAreNotSelfServe(t *testing.T) {
	// Business is sold by conversation; free needs no checkout. Both must refuse
	// rather than produce a broken session.
	for _, p := range []models.Plan{models.PlanFree, models.PlanBusiness, "nonsense"} {
		if _, err := PriceIDFor(p); err == nil {
			t.Fatalf("plan %q was offered a self-serve price", p)
		}
	}
}

// newSub builds a subscription object with one price and some metadata.
func newSub(priceID string, metadata map[string]any) subscriptionObject {
	var sub subscriptionObject
	sub.Metadata = metadata
	if priceID != "" || metadata == nil {
		item := struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
		}{}
		item.Price.ID = priceID
		sub.Items.Data = append(sub.Items.Data, item)
	}
	return sub
}
