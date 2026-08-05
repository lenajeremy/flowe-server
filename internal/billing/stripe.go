package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"workflow-ai/server/internal/billing/credits"
	"workflow-ai/server/internal/database/models"
)

// Stripe, for our own subscriptions.
//
// Not to be confused with the Stripe *integration* in internal/executor, which
// acts on a customer's own Stripe account through Connect. This file uses OUR
// platform secret key to sell OUR plans, and the two must never share a token.
//
// Raw HTTP with form encoding, matching the existing stripeCall helper, rather
// than pulling in the SDK for the four calls we make.
//
// # Location-based pricing
//
// Prices are created once in USD and Stripe's Adaptive Pricing presents them in
// the customer's local currency, converting at a rate guaranteed for 24 hours.
//
// There is deliberately NO adaptive_pricing parameter below, because it is not an
// API parameter: it is an account setting at
// dashboard.stripe.com/settings/adaptive-pricing and must be switched on there,
// per mode, or every customer simply sees USD.
//
// Two things here would silently disable it:
//
//   - Setting currency_options on a price for a currency the customer is in.
//     Adaptive Pricing yields to an explicit price, so hand-maintaining a few
//     currencies would turn off automatic conversion for exactly those markets.
//   - capture_method=manual, which we do not use.
//
// Cross-border subscriptions in a local currency support only card, Link, Apple
// Pay and Google Pay — a constraint of the feature, not of this code.

const stripeAPI = "https://api.stripe.com"

// stripeHTTP is separate from the integration client so our billing traffic is not
// subject to a customer-facing timeout policy.
var stripeHTTP = &http.Client{Timeout: 20 * time.Second}

func stripeKey() string     { return strings.TrimSpace(os.Getenv("STRIPE_SECRET_KEY")) }
func webhookSecret() string { return strings.TrimSpace(os.Getenv("STRIPE_WEBHOOK_SECRET")) }

// ErrStripeNotConfigured means billing is not set up on this deployment. Handlers
// turn it into a clear message rather than a 500, because a local dev server with
// no Stripe key is a normal state.
var ErrStripeNotConfigured = errors.New("billing is not configured on this server")

// priceEnv maps a plan to the environment variable holding its Stripe price id.
//
// Price ids rather than amounts: the amount lives in Stripe so it can be changed
// without a deploy, and so an existing subscriber keeps the price they signed up
// at. Hardcoding amounts here would make every price change a migration.
var priceEnv = map[models.Plan]string{
	models.PlanPro:  "STRIPE_PRICE_PRO",
	models.PlanTeam: "STRIPE_PRICE_TEAM",
}

// PriceIDFor returns the configured Stripe price for a plan.
func PriceIDFor(plan models.Plan) (string, error) {
	env, ok := priceEnv[plan]
	if !ok {
		// Business is sold by conversation, not by self-serve checkout, and free
		// needs no price at all.
		return "", fmt.Errorf("plan %q is not available for self-serve checkout", plan)
	}
	id := strings.TrimSpace(os.Getenv(env))
	if id == "" {
		return "", fmt.Errorf("%w: %s is not set", ErrStripeNotConfigured, env)
	}
	return id, nil
}

// stripePost calls the Stripe API with form-encoded parameters.
func stripePost(ctx context.Context, path string, form url.Values, idempotencyKey string) ([]byte, error) {
	key := stripeKey()
	if key == "" {
		return nil, ErrStripeNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		stripeAPI+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idempotencyKey != "" {
		// Without this, a retried checkout click can create a second subscription.
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := stripeHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stripe request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			return nil, fmt.Errorf("stripe %d: %s", resp.StatusCode, e.Error.Message)
		}
		return nil, fmt.Errorf("stripe %d", resp.StatusCode)
	}
	return raw, nil
}

func stripeGet(ctx context.Context, path string) ([]byte, error) {
	key := stripeKey()
	if key == "" {
		return nil, ErrStripeNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stripeAPI+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := stripeHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("stripe %d", resp.StatusCode)
	}
	return raw, nil
}

// ── Checkout ─────────────────────────────────────────────────────

// CheckoutSession is the subset of the response we use.
type CheckoutSession struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// StartCheckout creates a subscription Checkout Session for a plan.
func (g *Gate) StartCheckout(ctx context.Context, org *models.Organization, email string,
	plan models.Plan, seats int, successURL, cancelURL string) (*CheckoutSession, error) {

	price, err := PriceIDFor(plan)
	if err != nil {
		return nil, err
	}

	// Per-seat plans bill by quantity, with a floor so a "team" of one cannot
	// undercut Pro.
	quantity := 1
	if LimitsFor(plan).PerSeat {
		quantity = max(seats, MinSeats)
	}

	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("line_items[0][price]", price)
	form.Set("line_items[0][quantity]", strconv.Itoa(quantity))
	if LimitsFor(plan).PerSeat {
		// Let the customer change seat count on the Checkout page itself, rather than
		// forcing them back to pick a number before they have seen the price.
		form.Set("line_items[0][adjustable_quantity][enabled]", "true")
		form.Set("line_items[0][adjustable_quantity][minimum]", strconv.Itoa(MinSeats))
		form.Set("line_items[0][adjustable_quantity][maximum]", "200")
	}
	form.Set("success_url", successURL)
	form.Set("cancel_url", cancelURL)
	// Let Stripe collect the address it needs to localise and to compute tax.
	form.Set("billing_address_collection", "auto")
	form.Set("allow_promotion_codes", "true")
	// The org id travels on the SUBSCRIPTION, not just the session, because
	// subscription webhooks (renewals, cancellations) never mention the session
	// that created them. Without it, later events cannot be attributed.
	form.Set("subscription_data[metadata][organization_id]", org.ID.String())
	form.Set("subscription_data[metadata][plan]", string(plan))
	form.Set("metadata[organization_id]", org.ID.String())
	form.Set("metadata[plan]", string(plan))
	form.Set("client_reference_id", org.ID.String())

	// Reuse the Stripe customer if the org has one, so a second subscription does
	// not create a duplicate customer record and split their billing history.
	if org.StripeCustomerID != "" {
		form.Set("customer", org.StripeCustomerID)
		form.Set("customer_update[address]", "auto")
	} else if email != "" {
		form.Set("customer_email", email)
	}

	// Keyed on the org, plan and hour. Deduping a double-clicked upgrade button is
	// the point, but a key with no time component is worse than none: Stripe keeps a
	// key for 24 hours and a Checkout Session also expires in 24 hours, so a retry
	// the next day would get a cached response pointing at a dead URL.
	idem := fmt.Sprintf("checkout:%s:%s:%d:%d", org.ID.String(), plan, quantity, time.Now().Unix()/3600)
	raw, err := stripePost(ctx, "/v1/checkout/sessions", form, idem)
	if err != nil {
		return nil, err
	}
	var out CheckoutSession
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.URL == "" {
		return nil, fmt.Errorf("stripe returned a checkout session with no URL")
	}
	return &out, nil
}

// PortalURL opens the Stripe Customer Portal, where a customer changes their card,
// downloads invoices, switches plan or cancels.
//
// Using the hosted portal rather than building those flows is a deliberate
// trade: card handling and dunning are the parts of billing most expensive to get
// wrong, and Stripe already treats them as a product.
func (g *Gate) PortalURL(ctx context.Context, org *models.Organization, returnURL string) (string, error) {
	if org.StripeCustomerID == "" {
		return "", fmt.Errorf("this account has no billing history yet")
	}
	form := url.Values{}
	form.Set("customer", org.StripeCustomerID)
	form.Set("return_url", returnURL)
	raw, err := stripePost(ctx, "/v1/billing_portal/sessions", form, "")
	if err != nil {
		return "", err
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.URL, nil
}

// ── Webhook verification ─────────────────────────────────────────

// signatureTolerance rejects events whose timestamp is too old, which is what
// stops a captured request being replayed later. Stripe's own libraries default
// to five minutes.
const signatureTolerance = 5 * time.Minute

// VerifyWebhook authenticates a Stripe webhook from its Stripe-Signature header.
//
// The whole security of the billing webhook rests here: without verification
// anyone who learns the URL can grant themselves any plan. Two details matter and
// both are easy to get wrong —
//
//   - the signed payload is "{timestamp}.{raw body}", so the body must be the
//     EXACT bytes received, before any JSON round-trip;
//   - the comparison must be constant-time, or the response time leaks the
//     expected signature one byte at a time.
func VerifyWebhook(payload []byte, sigHeader string) error {
	secret := webhookSecret()
	if secret == "" {
		return fmt.Errorf("%w: STRIPE_WEBHOOK_SECRET is not set", ErrStripeNotConfigured)
	}

	var timestamp string
	var signatures []string
	for _, part := range strings.Split(sigHeader, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			timestamp = kv[1]
		case "v1":
			// A header can carry several v1 signatures during a secret rotation, and
			// any one of them matching is valid.
			signatures = append(signatures, kv[1])
		}
	}
	if timestamp == "" || len(signatures) == 0 {
		return errors.New("malformed Stripe-Signature header")
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("malformed timestamp in Stripe-Signature")
	}
	if age := time.Since(time.Unix(ts, 0)); age > signatureTolerance || age < -signatureTolerance {
		return fmt.Errorf("webhook timestamp outside tolerance (%s old)", age.Round(time.Second))
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(payload)
	expected := hex.EncodeToString(mac.Sum(nil))

	for _, got := range signatures {
		if hmac.Equal([]byte(expected), []byte(got)) {
			return nil
		}
	}
	return errors.New("webhook signature does not match")
}

// ── Webhook events ───────────────────────────────────────────────

// stripeEvent is the envelope every webhook arrives in.
type stripeEvent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

// subscriptionObject is the part of a Stripe subscription we act on.
type subscriptionObject struct {
	ID                 string         `json:"id"`
	Customer           string         `json:"customer"`
	Status             string         `json:"status"`
	CancelAtPeriodEnd  bool           `json:"cancel_at_period_end"`
	CurrentPeriodEnd   int64          `json:"current_period_end"`
	Metadata           map[string]any `json:"metadata"`
	PresentmentDetails *struct {
		PresentmentCurrency string `json:"presentment_currency"`
	} `json:"presentment_details"`
	Items struct {
		Data []struct {
			// Quantity is the seat count on a per-seat plan. Read from Stripe rather
			// than counted from org_members, so a team that has not finished inviting
			// people still gets the allowance it is paying for.
			Quantity int `json:"quantity"`
			Price    struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

// HandleWebhook applies one verified Stripe event.
//
// Returns nil for events we do not care about: Stripe retries anything that is
// not acknowledged, so treating an unknown type as an error would produce an
// endless retry loop over something we deliberately ignore.
func (g *Gate) HandleWebhook(ctx context.Context, payload []byte) error {
	var ev stripeEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return fmt.Errorf("unparseable webhook body: %w", err)
	}

	switch ev.Type {
	case "checkout.session.completed":
		return g.onCheckoutCompleted(ctx, ev)
	case "customer.subscription.created",
		"customer.subscription.updated",
		"customer.subscription.deleted":
		return g.onSubscriptionChanged(ev)
	case "invoice.paid":
		return g.onInvoicePaid(ev)
	}
	return nil
}

// onCheckoutCompleted links the org to its new Stripe customer and subscription.
//
// The session tells us WHICH org paid (via client_reference_id) but carries only
// summary subscription state, so the subscription itself is fetched and applied
// through the same path a later subscription webhook uses. That keeps one function
// responsible for deciding entitlement, rather than two that can disagree.
func (g *Gate) onCheckoutCompleted(ctx context.Context, ev stripeEvent) error {
	var sess struct {
		ID                string         `json:"id"`
		Customer          string         `json:"customer"`
		Subscription      string         `json:"subscription"`
		ClientReferenceID string         `json:"client_reference_id"`
		Metadata          map[string]any `json:"metadata"`
		CustomerDetails   *struct {
			Address *struct {
				Country string `json:"country"`
			} `json:"address"`
		} `json:"customer_details"`
		PresentmentDetails *struct {
			PresentmentCurrency string `json:"presentment_currency"`
		} `json:"presentment_details"`
	}
	if err := json.Unmarshal(ev.Data.Object, &sess); err != nil {
		return err
	}

	orgID := sess.ClientReferenceID
	if orgID == "" {
		if v, ok := sess.Metadata["organization_id"].(string); ok {
			orgID = v
		}
	}
	if orgID == "" {
		return fmt.Errorf("checkout session %s has no organization reference", sess.ID)
	}

	updates := map[string]any{}
	if sess.Customer != "" {
		updates["stripe_customer_id"] = sess.Customer
	}
	if sess.CustomerDetails != nil && sess.CustomerDetails.Address != nil {
		updates["billing_country"] = sess.CustomerDetails.Address.Country
	}
	if len(updates) > 0 {
		if err := g.db.Model(&models.Organization{}).
			Where("id = ?", orgID).Updates(updates).Error; err != nil {
			return err
		}
	}

	if sess.Subscription == "" {
		return nil
	}
	raw, err := stripeGet(ctx, "/v1/subscriptions/"+sess.Subscription)
	if err != nil {
		return err
	}
	var sub subscriptionObject
	if err := json.Unmarshal(raw, &sub); err != nil {
		return err
	}
	return g.applySubscription(orgID, sub, ev.ID)
}

// onSubscriptionChanged handles renewals, plan changes and cancellations.
func (g *Gate) onSubscriptionChanged(ev stripeEvent) error {
	var sub subscriptionObject
	if err := json.Unmarshal(ev.Data.Object, &sub); err != nil {
		return err
	}
	orgID, _ := sub.Metadata["organization_id"].(string)
	if orgID == "" {
		// Fall back to the customer link. A subscription created outside our
		// checkout — someone provisioning by hand in the Dashboard — has no
		// metadata, and refusing it would make manual provisioning impossible.
		var org models.Organization
		if err := g.db.Where("stripe_customer_id = ?", sub.Customer).First(&org).Error; err != nil {
			return fmt.Errorf("subscription %s belongs to no known organization", sub.ID)
		}
		orgID = org.ID.String()
	}
	if ev.Type == "customer.subscription.deleted" {
		// Ended outright. Entitlement still respects current_period_end, which
		// EffectivePlan enforces — the customer paid for this period.
		return g.db.Model(&models.Organization{}).Where("id = ?", orgID).
			Updates(map[string]any{"plan_status": "canceled", "cancel_at_period_end": true}).Error
	}
	return g.applySubscription(orgID, sub, ev.ID)
}

// applySubscription writes subscription state onto the org and grants the plan's
// credits for the new period.
func (g *Gate) applySubscription(orgID string, sub subscriptionObject, eventID string) error {
	plan := planFromSubscription(sub)
	seats := seatsFromSubscription(sub)

	updates := map[string]any{
		"stripe_subscription_id": sub.ID,
		"plan_status":            sub.Status,
		"cancel_at_period_end":   sub.CancelAtPeriodEnd,
		"seats":                  seats,
	}
	if sub.Customer != "" {
		updates["stripe_customer_id"] = sub.Customer
	}
	if plan != "" {
		updates["plan"] = string(plan)
	}
	if sub.CurrentPeriodEnd > 0 {
		updates["current_period_end"] = time.Unix(sub.CurrentPeriodEnd, 0)
	}
	if err := g.db.Model(&models.Organization{}).
		Where("id = ?", orgID).Updates(updates).Error; err != nil {
		return err
	}

	// Credits for the period. Keyed on the subscription and period rather than on
	// the event id, so the several events that can describe one period (created,
	// then updated) grant exactly once.
	if plan != "" && (sub.Status == "active" || sub.Status == "trialing") {
		// The seat count is part of the reference, so ADDING seats mid-period grants
		// the difference rather than being swallowed as a duplicate of the original
		// period's grant. Stripe prorates the charge; the allowance has to follow.
		ref := fmt.Sprintf("sub:%s:period:%d:seats:%d", sub.ID, sub.CurrentPeriodEnd, seats)
		if err := credits.Grant(g.db, orgID, planGrantForSeats(plan, seats),
			models.ReasonMonthlyGrant, ref); err != nil {
			return err
		}
	}
	return nil
}

// seatsFromSubscription reads the billed quantity, defaulting to one.
//
// A subscription with no quantity — a flat plan, or a hand-made one — is a single
// seat rather than zero, because zero would leave a paying customer with no
// allowance at all.
func seatsFromSubscription(sub subscriptionObject) int {
	if len(sub.Items.Data) > 0 && sub.Items.Data[0].Quantity > 0 {
		return sub.Items.Data[0].Quantity
	}
	return 1
}

// onInvoicePaid grants the next period's credits on renewal.
func (g *Gate) onInvoicePaid(ev stripeEvent) error {
	var inv struct {
		ID            string `json:"id"`
		Customer      string `json:"customer"`
		Subscription  string `json:"subscription"`
		BillingReason string `json:"billing_reason"`
		Lines         struct {
			Data []struct {
				Period struct {
					End int64 `json:"end"`
				} `json:"period"`
			} `json:"data"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(ev.Data.Object, &inv); err != nil {
		return err
	}
	// Only a renewal needs a fresh grant; the first invoice is already covered by
	// the subscription event, and the shared external ref prevents a double grant
	// either way.
	if inv.BillingReason != "subscription_cycle" || inv.Subscription == "" {
		return nil
	}
	var org models.Organization
	if err := g.db.Where("stripe_subscription_id = ?", inv.Subscription).First(&org).Error; err != nil {
		return nil // not ours
	}
	plan := EffectivePlan(&org)
	if plan == models.PlanFree {
		return nil
	}
	var periodEnd int64
	if len(inv.Lines.Data) > 0 {
		periodEnd = inv.Lines.Data[0].Period.End
	}
	seats := max(org.Seats, 1)
	ref := fmt.Sprintf("sub:%s:period:%d:seats:%d", inv.Subscription, periodEnd, seats)
	return credits.Grant(g.db, org.ID.String(), planGrantForSeats(plan, seats),
		models.ReasonMonthlyGrant, ref)
}

// planFromSubscription resolves which plan a subscription represents.
//
// Metadata first, since our own checkout sets it. The price id is the fallback,
// which is what makes a subscription created by hand in the Stripe Dashboard work
// — and a plan changed through the Customer Portal, where the new price arrives
// without updated metadata.
func planFromSubscription(sub subscriptionObject) models.Plan {
	if len(sub.Items.Data) > 0 {
		priceID := sub.Items.Data[0].Price.ID
		for plan, env := range priceEnv {
			if configured := strings.TrimSpace(os.Getenv(env)); configured != "" && configured == priceID {
				return plan
			}
		}
	}
	if v, ok := sub.Metadata["plan"].(string); ok {
		if _, known := planLimits[models.Plan(v)]; known {
			return models.Plan(v)
		}
	}
	return ""
}

// planGrantForSeats is the credit allowance for a plan at a seat count, indirected
// through the limits table so the pricing page and the grant cannot disagree.
func planGrantForSeats(plan models.Plan, seats int) int64 {
	return Scale(LimitsFor(plan), seats).MonthlyCredits
}
