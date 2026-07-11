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
