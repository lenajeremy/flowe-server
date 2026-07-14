package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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

	case "get_product":
		return shopifyCall(ctx, token, shop, http.MethodGet, "/products/"+sub(d.ShopifyProductId)+".json", nil)

	case "update_product":
		product := map[string]any{"id": sub(d.ShopifyProductId)}
		if v := sub(d.ShopifyTitle); v != "" {
			product["title"] = v
		}
		if v := sub(d.ShopifyDescription); v != "" {
			product["body_html"] = v
		}
		if price := sub(d.ShopifyPrice); price != "" {
			product["variants"] = []any{map[string]any{"price": price}}
		}
		raw, err := shopifyCall(ctx, token, shop, http.MethodPut,
			"/products/"+sub(d.ShopifyProductId)+".json", map[string]any{"product": product})
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 4000), nil

	case "delete_product":
		if _, err := shopifyCall(ctx, token, shop, http.MethodDelete,
			"/products/"+sub(d.ShopifyProductId)+".json", nil); err != nil {
			return "", err
		}
		return `{"status":"deleted"}`, nil

	case "cancel_order":
		raw, err := shopifyCall(ctx, token, shop, http.MethodPost,
			"/orders/"+sub(d.ShopifyOrderId)+"/cancel.json", map[string]any{})
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 4000), nil

	case "close_order":
		raw, err := shopifyCall(ctx, token, shop, http.MethodPost,
			"/orders/"+sub(d.ShopifyOrderId)+"/close.json", map[string]any{})
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 4000), nil

	case "create_customer":
		names := strings.SplitN(strings.TrimSpace(sub(d.ShopifyCustomerName)), " ", 2)
		customer := map[string]any{"email": sub(d.ShopifyCustomerEmail), "first_name": names[0]}
		if len(names) > 1 {
			customer["last_name"] = names[1]
		}
		raw, err := shopifyCall(ctx, token, shop, http.MethodPost,
			"/customers.json", map[string]any{"customer": customer})
		if err != nil {
			return "", err
		}
		var res struct {
			Customer struct {
				ID    int64  `json:"id"`
				Email string `json:"email"`
			} `json:"customer"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		b, _ := json.Marshal(map[string]any{"status": "created", "id": res.Customer.ID, "email": res.Customer.Email})
		return string(b), nil

	case "get_customer":
		return shopifyCall(ctx, token, shop, http.MethodGet, "/customers/"+sub(d.ShopifyCustomerId)+".json", nil)

	case "search_customers":
		q := url.Values{"query": {sub(d.ShopifyQuery)}, "limit": {limit}}
		raw, err := shopifyCall(ctx, token, shop, http.MethodGet, "/customers/search.json?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "create_draft_order":
		item := map[string]any{
			"title":    firstNonEmpty(sub(d.ShopifyTitle), "Custom item"),
			"price":    firstNonEmpty(sub(d.ShopifyPrice), "0.00"),
			"quantity": intOr(d.ShopifyQuantity, 1),
		}
		draft := map[string]any{"line_items": []any{item}}
		if email := sub(d.ShopifyCustomerEmail); email != "" {
			draft["email"] = email
		}
		raw, err := shopifyCall(ctx, token, shop, http.MethodPost,
			"/draft_orders.json", map[string]any{"draft_order": draft})
		if err != nil {
			return "", err
		}
		var res struct {
			DraftOrder struct {
				ID         int64  `json:"id"`
				InvoiceURL string `json:"invoice_url"`
			} `json:"draft_order"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		b, _ := json.Marshal(map[string]any{"status": "created", "id": res.DraftOrder.ID, "invoice_url": res.DraftOrder.InvoiceURL})
		return string(b), nil

	case "list_draft_orders":
		raw, err := shopifyCall(ctx, token, shop, http.MethodGet, "/draft_orders.json?limit="+limit, nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "list_locations":
		raw, err := shopifyCall(ctx, token, shop, http.MethodGet, "/locations.json", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 8000), nil

	case "adjust_inventory":
		if d.ShopifyDelta == 0 {
			return "", fmt.Errorf("Shopify: shopifyDelta must be a non-zero adjustment")
		}
		raw, err := shopifyCall(ctx, token, shop, http.MethodPost, "/inventory_levels/adjust.json", map[string]any{
			"inventory_item_id":    sub(d.ShopifyInventoryItemId),
			"location_id":          sub(d.ShopifyLocationId),
			"available_adjustment": d.ShopifyDelta,
		})
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 2000), nil

	case "create_discount_code":
		dtype := firstNonEmpty(d.ShopifyDiscountType, "percentage")
		value := sub(d.ShopifyDiscountValue)
		// Shopify price-rule values are negative ("-10" = 10% or $10 off).
		if !strings.HasPrefix(value, "-") {
			value = "-" + value
		}
		ruleRaw, err := shopifyCall(ctx, token, shop, http.MethodPost, "/price_rules.json", map[string]any{
			"price_rule": map[string]any{
				"title":              sub(d.ShopifyDiscountCode),
				"target_type":        "line_item",
				"target_selection":   "all",
				"allocation_method":  "across",
				"value_type":         dtype,
				"value":              value,
				"customer_selection": "all",
				"starts_at":          time.Now().Format(time.RFC3339),
			},
		})
		if err != nil {
			return "", err
		}
		var rule struct {
			PriceRule struct {
				ID int64 `json:"id"`
			} `json:"price_rule"`
		}
		_ = json.Unmarshal([]byte(ruleRaw), &rule)
		if rule.PriceRule.ID == 0 {
			return "", fmt.Errorf("Shopify: price rule creation failed")
		}
		if _, err := shopifyCall(ctx, token, shop, http.MethodPost,
			fmt.Sprintf("/price_rules/%d/discount_codes.json", rule.PriceRule.ID),
			map[string]any{"discount_code": map[string]any{"code": sub(d.ShopifyDiscountCode)}}); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": "created", "code": sub(d.ShopifyDiscountCode), "price_rule_id": rule.PriceRule.ID})
		return string(b), nil

	default:
		return "", fmt.Errorf("unknown Shopify operation: %s", d.IntegrationOp)
	}
}
