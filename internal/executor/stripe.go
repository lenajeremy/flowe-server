package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Stripe API. Bearer auth with the connected account's access token;
// request bodies are form-encoded.

func stripeCall(ctx context.Context, token, method, path string, form url.Values) (string, error) {
	var reader io.Reader
	if form != nil {
		reader = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.stripe.com"+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("stripe request failed: %w", err)
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
			return "", fmt.Errorf("Stripe API error (%d): %s", resp.StatusCode, e.Error.Message)
		}
		return "", fmt.Errorf("Stripe API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return string(raw), nil
}

func runStripe(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := fmt.Sprint(intOr(d.StripeLimit, 10))

	switch d.IntegrationOp {
	case "list_customers":
		q := url.Values{"limit": {limit}}
		if email := sub(d.StripeCustomerEmail); email != "" {
			q.Set("email", email)
		}
		return stripeList(ctx, token, "/v1/customers?"+q.Encode())

	case "list_payments":
		return stripeList(ctx, token, "/v1/payment_intents?limit="+limit)

	case "list_invoices":
		return stripeList(ctx, token, "/v1/invoices?limit="+limit)

	case "get_balance":
		return stripeCall(ctx, token, http.MethodGet, "/v1/balance", nil)

	case "create_payment_link":
		price := sub(d.StripePriceId)
		if price == "" {
			return "", fmt.Errorf("stripePriceId is required to create a payment link")
		}
		form := url.Values{}
		form.Set("line_items[0][price]", price)
		form.Set("line_items[0][quantity]", fmt.Sprint(intOr(d.StripeQuantity, 1)))
		raw, err := stripeCall(ctx, token, http.MethodPost, "/v1/payment_links", form)
		if err != nil {
			return "", err
		}
		var link struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(raw), &link) == nil && link.URL != "" {
			b, _ := json.Marshal(map[string]any{"status": "created", "id": link.ID, "url": link.URL})
			return string(b), nil
		}
		return raw, nil

	case "create_customer":
		form := url.Values{}
		if email := sub(d.StripeCustomerEmail); email != "" {
			form.Set("email", email)
		}
		if name := sub(d.StripeCustomerName); name != "" {
			form.Set("name", name)
		}
		raw, err := stripeCall(ctx, token, http.MethodPost, "/v1/customers", form)
		if err != nil {
			return "", err
		}
		var c struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		}
		_ = json.Unmarshal([]byte(raw), &c)
		b, _ := json.Marshal(map[string]any{"status": "created", "id": c.ID, "email": c.Email})
		return string(b), nil

	case "get_customer":
		return stripeList(ctx, token, "/v1/customers/"+url.PathEscape(sub(d.StripeCustomerId)))

	case "list_subscriptions":
		q := url.Values{"limit": {limit}}
		if cust := sub(d.StripeCustomerId); cust != "" {
			q.Set("customer", cust)
		}
		return stripeList(ctx, token, "/v1/subscriptions?"+q.Encode())

	case "get_subscription":
		return stripeList(ctx, token, "/v1/subscriptions/"+url.PathEscape(sub(d.StripeSubscriptionId)))

	case "cancel_subscription":
		raw, err := stripeCall(ctx, token, http.MethodDelete,
			"/v1/subscriptions/"+url.PathEscape(sub(d.StripeSubscriptionId)), nil)
		if err != nil {
			return "", err
		}
		var sc struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal([]byte(raw), &sc)
		b, _ := json.Marshal(map[string]any{"status": sc.Status, "id": sc.ID})
		return string(b), nil

	case "list_products":
		return stripeList(ctx, token, "/v1/products?limit="+limit)

	case "create_product":
		form := url.Values{"name": {sub(d.StripeProductName)}}
		raw, err := stripeCall(ctx, token, http.MethodPost, "/v1/products", form)
		if err != nil {
			return "", err
		}
		var pr struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal([]byte(raw), &pr)
		b, _ := json.Marshal(map[string]any{"status": "created", "id": pr.ID, "name": pr.Name})
		return string(b), nil

	case "create_price":
		if d.StripeAmount <= 0 {
			return "", fmt.Errorf("Stripe: stripeAmount (cents) must be > 0")
		}
		form := url.Values{}
		form.Set("product", sub(d.StripeProductId))
		form.Set("unit_amount", fmt.Sprint(d.StripeAmount))
		form.Set("currency", firstNonEmpty(sub(d.StripeCurrency), "usd"))
		if iv := d.StripeInterval; iv == "month" || iv == "year" {
			form.Set("recurring[interval]", iv)
		}
		raw, err := stripeCall(ctx, token, http.MethodPost, "/v1/prices", form)
		if err != nil {
			return "", err
		}
		var pc struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &pc)
		b, _ := json.Marshal(map[string]any{"status": "created", "id": pc.ID})
		return string(b), nil

	case "get_invoice":
		return stripeList(ctx, token, "/v1/invoices/"+url.PathEscape(sub(d.StripeInvoiceId)))

	case "get_payment_intent":
		return stripeList(ctx, token, "/v1/payment_intents/"+url.PathEscape(sub(d.StripePaymentIntentId)))

	case "create_refund":
		form := url.Values{"payment_intent": {sub(d.StripePaymentIntentId)}}
		if d.StripeAmount > 0 {
			form.Set("amount", fmt.Sprint(d.StripeAmount)) // partial refund, cents
		}
		if r := d.StripeRefundReason; r == "duplicate" || r == "fraudulent" || r == "requested_by_customer" {
			form.Set("reason", r)
		}
		raw, err := stripeCall(ctx, token, http.MethodPost, "/v1/refunds", form)
		if err != nil {
			return "", err
		}
		var rf struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Amount int    `json:"amount"`
		}
		_ = json.Unmarshal([]byte(raw), &rf)
		b, _ := json.Marshal(map[string]any{"status": rf.Status, "id": rf.ID, "amount": rf.Amount})
		return string(b), nil

	case "list_refunds":
		return stripeList(ctx, token, "/v1/refunds?limit="+limit)

	case "list_events":
		return stripeList(ctx, token, "/v1/events?limit="+limit)

	default:
		return "", fmt.Errorf("unknown Stripe operation: %s", d.IntegrationOp)
	}
}

func stripeList(ctx context.Context, token, path string) (string, error) {
	raw, err := stripeCall(ctx, token, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	return truncateStr(raw, 8000), nil
}
