package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Shopify Admin REST API. The shop domain is the per-tenant base; auth is the
// X-Shopify-Access-Token header.

func shopifyCall(ctx context.Context, token, shop, method, path string, body any) (string, error) {
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+shop+"/admin/api/2024-01"+path, reader)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Shopify-Access-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("shopify request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Errors any `json:"errors"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Errors != nil {
			return "", fmt.Errorf("Shopify API error (%d): %v", resp.StatusCode, e.Errors)
		}
		return "", fmt.Errorf("Shopify API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	return string(raw), nil
}

func runShopify(ctx context.Context, token, shop string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	limit := fmt.Sprint(intOr(d.ShopifyLimit, 10))

	switch d.IntegrationOp {
	case "list_orders":
		q := url.Values{"limit": {limit}, "status": {firstNonEmpty(d.ShopifyStatus, "any")}}
		raw, err := shopifyCall(ctx, token, shop, http.MethodGet, "/orders.json?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "get_order":
		return shopifyCall(ctx, token, shop, http.MethodGet, "/orders/"+sub(d.ShopifyOrderId)+".json", nil)

	case "list_products":
		raw, err := shopifyCall(ctx, token, shop, http.MethodGet, "/products.json?limit="+limit, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "create_product":
		product := map[string]any{
			"title":     sub(d.ShopifyTitle),
			"body_html": sub(d.ShopifyDescription),
		}
		if price := sub(d.ShopifyPrice); price != "" {
			product["variants"] = []any{map[string]any{"price": price}}
		}
		raw, err := shopifyCall(ctx, token, shop, http.MethodPost, "/products.json", map[string]any{"product": product})
		if err != nil {
			return "", err
		}
		var res struct {
			Product struct {
				ID     int64  `json:"id"`
				Title  string `json:"title"`
				Handle string `json:"handle"`
			} `json:"product"`
		}
		if json.Unmarshal([]byte(raw), &res) == nil && res.Product.ID != 0 {
			b, _ := json.Marshal(map[string]any{"status": "created", "id": res.Product.ID, "title": res.Product.Title})
			return string(b), nil
		}
		return raw, nil

	case "list_customers":
		raw, err := shopifyCall(ctx, token, shop, http.MethodGet, "/customers.json?limit="+limit, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	default:
		return "", fmt.Errorf("unknown Shopify operation: %s", d.IntegrationOp)
	}
}
