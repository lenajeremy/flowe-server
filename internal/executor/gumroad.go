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

// Gumroad API v2.
//
// Two shape notes that matter when reading the ops:
//
//   - Almost everything is nested under a product, including offer codes, custom
//     fields, variants and subscribers. Only sales, licenses, webhooks and the
//     user live at the top level.
//   - Gumroad wraps every response in {"success": true, ...} and reports
//     failures with the same envelope and a message rather than always using an
//     HTTP status, so a 200 is checked for success:false before being returned.
//
// Mutations are form-encoded rather than JSON, which is easy to miss: sending a
// JSON body silently produces a request Gumroad ignores the fields of.

const gumroadAPI = "https://api.gumroad.com/v2"

// gumroadCall performs a request. Values are form-encoded for writes, which is
// what the API expects; nil means no parameters.
func gumroadCall(ctx context.Context, token, method, path string, form url.Values) (string, error) {
	var body io.Reader
	target := gumroadAPI + path
	if len(form) > 0 {
		if method == http.MethodGet {
			target += "?" + form.Encode()
		} else {
			body = strings.NewReader(form.Encode())
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("gumroad request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	// Gumroad answers 200 with success:false for a rejected request, so the
	// envelope has to be inspected rather than trusting the status alone.
	var env struct {
		Success *bool  `json:"success"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	failed := resp.StatusCode < 200 || resp.StatusCode >= 300 ||
		(env.Success != nil && !*env.Success)
	if failed {
		msg := firstNonEmpty(env.Message, env.Error, truncateStr(string(raw), 300))
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			msg += " — reconnect Gumroad; the token may have been revoked"
		case http.StatusForbidden:
			msg += " — the connected Gumroad application may be missing a permission for this call"
		}
		return "", fmt.Errorf("Gumroad API error (%d): %s", resp.StatusCode, msg)
	}
	return string(raw), nil
}

func runGumroad(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	product := func() string { return url.PathEscape(sub(d.GumroadProductId)) }
	need := func(label, v string) error {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("this operation needs %s", label)
		}
		return nil
	}
	// A product's price is in cents, and sending currency units silently sells
	// something for a hundredth of its intended price.
	cents := func(v string) (int, error) {
		n, err := atoiSafe(v)
		if err != nil {
			return 0, fmt.Errorf("price must be a whole number of cents — 1000 is $10.00")
		}
		return n, nil
	}

	switch d.IntegrationOp {
	// ---- user ----
	case "get_user":
		return gumroadCall(ctx, token, http.MethodGet, "/user", nil)

	// ---- products ----
	case "list_products":
		raw, err := gumroadCall(ctx, token, http.MethodGet, "/products", nil)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "get_product":
		if err := need("a product ID", sub(d.GumroadProductId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodGet, "/products/"+product(), nil)

	case "create_product":
		if err := need("a name", sub(d.GumroadName)); err != nil {
			return "", err
		}
		price, err := cents(sub(d.GumroadPrice))
		if err != nil {
			return "", err
		}
		form := url.Values{
			"name":  {sub(d.GumroadName)},
			"price": {fmt.Sprint(price)},
		}
		if v := sub(d.GumroadDescription); v != "" {
			form.Set("description", v)
		}
		if v := sub(d.GumroadUrl); v != "" {
			form.Set("url", v)
		}
		if v := sub(d.GumroadCustomPermalink); v != "" {
			form.Set("custom_permalink", v)
		}
		return gumroadCall(ctx, token, http.MethodPost, "/products", form)

	case "update_product":
		if err := need("a product ID", sub(d.GumroadProductId)); err != nil {
			return "", err
		}
		form := url.Values{}
		if v := sub(d.GumroadName); v != "" {
			form.Set("name", v)
		}
		if v := sub(d.GumroadDescription); v != "" {
			form.Set("description", v)
		}
		if v := sub(d.GumroadPrice); v != "" {
			price, err := cents(v)
			if err != nil {
				return "", err
			}
			form.Set("price", fmt.Sprint(price))
		}
		if len(form) == 0 {
			return "", fmt.Errorf("update_product needs at least one field to change")
		}
		return gumroadCall(ctx, token, http.MethodPut, "/products/"+product(), form)

	case "delete_product":
		if err := need("a product ID", sub(d.GumroadProductId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodDelete, "/products/"+product(), nil)

	case "enable_product":
		if err := need("a product ID", sub(d.GumroadProductId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodPut, "/products/"+product()+"/enable", nil)

	case "disable_product":
		if err := need("a product ID", sub(d.GumroadProductId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodPut, "/products/"+product()+"/disable", nil)

	// ---- variants ----
	case "list_variant_categories":
		if err := need("a product ID", sub(d.GumroadProductId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodGet,
			"/products/"+product()+"/variant_categories", nil)

	case "create_variant_category":
		if err := need("a product ID and a title", sub(d.GumroadProductId)+sub(d.GumroadTitle)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodPost,
			"/products/"+product()+"/variant_categories",
			url.Values{"title": {sub(d.GumroadTitle)}})

	case "list_variants":
		if err := need("a product ID", sub(d.GumroadProductId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodGet, "/products/"+product()+"/variants", nil)

	case "create_variant":
		if err := need("a product ID, a category ID and a name",
			sub(d.GumroadProductId)+sub(d.GumroadCategoryId)+sub(d.GumroadName)); err != nil {
			return "", err
		}
		form := url.Values{"name": {sub(d.GumroadName)}}
		if v := sub(d.GumroadPriceDifference); v != "" {
			diff, err := cents(v)
			if err != nil {
				return "", err
			}
			// The surcharge for this variant, again in cents.
			form.Set("price_difference_cents", fmt.Sprint(diff))
		}
		return gumroadCall(ctx, token, http.MethodPost, fmt.Sprintf(
			"/products/%s/variant_categories/%s/variants",
			product(), url.PathEscape(sub(d.GumroadCategoryId))), form)

	// ---- offer codes ----
	case "list_offer_codes":
		if err := need("a product ID", sub(d.GumroadProductId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodGet, "/products/"+product()+"/offer_codes", nil)

	case "get_offer_code":
		if err := need("a product ID and an offer code ID",
			sub(d.GumroadProductId)+sub(d.GumroadOfferCodeId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodGet,
			"/products/"+product()+"/offer_codes/"+url.PathEscape(sub(d.GumroadOfferCodeId)), nil)

	case "create_offer_code":
		if err := need("a product ID and a code", sub(d.GumroadProductId)+sub(d.GumroadCode)); err != nil {
			return "", err
		}
		if sub(d.GumroadAmountOff) == "" {
			return "", fmt.Errorf("create_offer_code needs an amount off")
		}
		amount, err := atoiSafe(sub(d.GumroadAmountOff))
		if err != nil {
			return "", fmt.Errorf("amount off must be a whole number — cents for a fixed " +
				"discount, or a percentage when the type is percent")
		}
		form := url.Values{
			"name":       {sub(d.GumroadCode)},
			"amount_off": {fmt.Sprint(amount)},
		}
		// Gumroad decides fixed-vs-percentage from this flag, not from the number.
		if strings.EqualFold(sub(d.GumroadOfferType), "percent") {
			form.Set("offer_type", "percent")
		} else {
			form.Set("offer_type", "cents")
		}
		if v := sub(d.GumroadMaxPurchases); v != "" {
			form.Set("max_purchase_count", v)
		}
		return gumroadCall(ctx, token, http.MethodPost,
			"/products/"+product()+"/offer_codes", form)

	case "update_offer_code":
		if err := need("a product ID and an offer code ID",
			sub(d.GumroadProductId)+sub(d.GumroadOfferCodeId)); err != nil {
			return "", err
		}
		form := url.Values{}
		if v := sub(d.GumroadMaxPurchases); v != "" {
			form.Set("max_purchase_count", v)
		}
		if len(form) == 0 {
			return "", fmt.Errorf("update_offer_code can only change the maximum purchase count")
		}
		return gumroadCall(ctx, token, http.MethodPut,
			"/products/"+product()+"/offer_codes/"+url.PathEscape(sub(d.GumroadOfferCodeId)), form)

	case "delete_offer_code":
		if err := need("a product ID and an offer code ID",
			sub(d.GumroadProductId)+sub(d.GumroadOfferCodeId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodDelete,
			"/products/"+product()+"/offer_codes/"+url.PathEscape(sub(d.GumroadOfferCodeId)), nil)

	// ---- custom fields ----
	case "list_custom_fields":
		if err := need("a product ID", sub(d.GumroadProductId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodGet, "/products/"+product()+"/custom_fields", nil)

	case "create_custom_field":
		if err := need("a product ID and a name", sub(d.GumroadProductId)+sub(d.GumroadName)); err != nil {
			return "", err
		}
		form := url.Values{"name": {sub(d.GumroadName)}}
		if v := sub(d.GumroadRequired); v != "" {
			form.Set("required", fmt.Sprint(strings.EqualFold(v, "true")))
		}
		return gumroadCall(ctx, token, http.MethodPost,
			"/products/"+product()+"/custom_fields", form)

	case "delete_custom_field":
		if err := need("a product ID and a field name",
			sub(d.GumroadProductId)+sub(d.GumroadName)); err != nil {
			return "", err
		}
		// Custom fields are addressed by name, not by a generated id.
		return gumroadCall(ctx, token, http.MethodDelete,
			"/products/"+product()+"/custom_fields/"+url.PathEscape(sub(d.GumroadName)), nil)

	// ---- sales ----
	case "list_sales":
		form := url.Values{}
		if v := sub(d.GumroadAfter); v != "" {
			// YYYY-MM-DD, and the bound is exclusive.
			form.Set("after", v)
		}
		if v := sub(d.GumroadBefore); v != "" {
			form.Set("before", v)
		}
		if v := sub(d.GumroadProductId); v != "" {
			form.Set("product_id", v)
		}
		if v := sub(d.GumroadEmail); v != "" {
			form.Set("email", v)
		}
		if v := sub(d.GumroadPageKey); v != "" {
			// Gumroad pages sales with an opaque key rather than an offset.
			form.Set("page_key", v)
		}
		raw, err := gumroadCall(ctx, token, http.MethodGet, "/sales", form)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "get_sale":
		if err := need("a sale ID", sub(d.GumroadSaleId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodGet,
			"/sales/"+url.PathEscape(sub(d.GumroadSaleId)), nil)

	case "mark_as_shipped":
		if err := need("a sale ID", sub(d.GumroadSaleId)); err != nil {
			return "", err
		}
		form := url.Values{}
		if v := sub(d.GumroadTrackingUrl); v != "" {
			form.Set("tracking_url", v)
		}
		return gumroadCall(ctx, token, http.MethodPut,
			"/sales/"+url.PathEscape(sub(d.GumroadSaleId))+"/mark_as_shipped", form)

	case "refund_sale":
		if err := need("a sale ID", sub(d.GumroadSaleId)); err != nil {
			return "", err
		}
		form := url.Values{}
		if v := sub(d.GumroadAmount); v != "" {
			// Omitting the amount refunds the sale in full.
			amount, err := cents(v)
			if err != nil {
				return "", err
			}
			form.Set("amount_cents", fmt.Sprint(amount))
		}
		return gumroadCall(ctx, token, http.MethodPut,
			"/sales/"+url.PathEscape(sub(d.GumroadSaleId))+"/refund", form)

	// ---- subscribers ----
	case "list_subscribers":
		if err := need("a product ID", sub(d.GumroadProductId)); err != nil {
			return "", err
		}
		form := url.Values{}
		if v := sub(d.GumroadEmail); v != "" {
			form.Set("email", v)
		}
		raw, err := gumroadCall(ctx, token, http.MethodGet,
			"/products/"+product()+"/subscribers", form)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "get_subscriber":
		if err := need("a subscriber ID", sub(d.GumroadSubscriberId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodGet,
			"/subscribers/"+url.PathEscape(sub(d.GumroadSubscriberId)), nil)

	// ---- licenses ----
	case "verify_license":
		if err := need("a product ID and a license key",
			sub(d.GumroadProductId)+sub(d.GumroadLicenseKey)); err != nil {
			return "", err
		}
		form := url.Values{
			"product_id":  {sub(d.GumroadProductId)},
			"license_key": {sub(d.GumroadLicenseKey)},
		}
		// Verifying normally counts a use; this keeps a read-only check read-only.
		if !strings.EqualFold(sub(d.GumroadIncrementUses), "true") {
			form.Set("increment_uses_count", "false")
		}
		return gumroadCall(ctx, token, http.MethodPost, "/licenses/verify", form)

	case "enable_license", "disable_license", "decrement_license_uses":
		if err := need("a product ID and a license key",
			sub(d.GumroadProductId)+sub(d.GumroadLicenseKey)); err != nil {
			return "", err
		}
		path := map[string]string{
			"enable_license":         "/licenses/enable",
			"disable_license":        "/licenses/disable",
			"decrement_license_uses": "/licenses/decrement_uses_count",
		}[d.IntegrationOp]
		return gumroadCall(ctx, token, http.MethodPut, path, url.Values{
			"product_id":  {sub(d.GumroadProductId)},
			"license_key": {sub(d.GumroadLicenseKey)},
		})

	// ---- webhooks ----
	case "list_webhooks":
		form := url.Values{}
		if v := sub(d.GumroadResourceName); v != "" {
			form.Set("resource_name", v)
		}
		return gumroadCall(ctx, token, http.MethodGet, "/resource_subscriptions", form)

	case "create_webhook":
		if err := need("a resource name and a post URL",
			sub(d.GumroadResourceName)+sub(d.GumroadUrl)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodPut, "/resource_subscriptions", url.Values{
			"resource_name": {sub(d.GumroadResourceName)},
			"post_url":      {sub(d.GumroadUrl)},
		})

	case "delete_webhook":
		if err := need("a webhook ID", sub(d.GumroadWebhookId)); err != nil {
			return "", err
		}
		return gumroadCall(ctx, token, http.MethodDelete,
			"/resource_subscriptions/"+url.PathEscape(sub(d.GumroadWebhookId)), nil)

	case "":
		return "", fmt.Errorf("no Gumroad operation selected")
	}
	return "", fmt.Errorf("unsupported Gumroad operation: %s", d.IntegrationOp)
}
