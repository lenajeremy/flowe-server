package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Google Search Console.
//
// The awkward part is the property identifier. A site is addressed by its own URL
// sitting inside the request path, so it has to be percent-encoded twice over as
// far as a reader is concerned: "https://example.com/" becomes
// "https%3A%2F%2Fexample.com%2F". A Domain property is written "sc-domain:example.com"
// instead. Both forms go through gscProperty, and getting this wrong is the usual
// cause of a 403 that looks like a permissions problem.
//
// Note also that Search Console reports in Pacific time and lags roughly two days
// behind, so "yesterday" often has no data yet. That is a property of the product
// rather than something to work around here, but it explains empty results.

const (
	gscAPI        = "https://www.googleapis.com/webmasters/v3"
	gscInspectAPI = "https://searchconsole.googleapis.com/v1"
)

// gscProperty encodes a property identifier for use as a path segment.
func gscProperty(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	// A domain property is already opaque; a URL-prefix property needs encoding.
	return url.PathEscape(v)
}

func runGoogleSearchConsole(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	site := func() string { return gscProperty(sub(d.GscSiteUrl)) }
	needSite := func() error {
		if sub(d.GscSiteUrl) == "" {
			return fmt.Errorf("this operation needs a property — either a URL like " +
				"https://example.com/ or a domain property like sc-domain:example.com")
		}
		return nil
	}

	switch d.IntegrationOp {
	// ---- properties ----
	case "list_sites":
		return googleCall(ctx, token, http.MethodGet, gscAPI+"/sites", nil)

	case "get_site":
		if err := needSite(); err != nil {
			return "", err
		}
		return googleCall(ctx, token, http.MethodGet, gscAPI+"/sites/"+site(), nil)

	case "add_site":
		// PUT with no body: the property is the whole request.
		if err := needSite(); err != nil {
			return "", err
		}
		if _, err := googleCall(ctx, token, http.MethodPut, gscAPI+"/sites/"+site(), nil); err != nil {
			return "", err
		}
		// Adding a property does not verify it; that still has to happen in the UI.
		return fmt.Sprintf(`{"ok":true,"site":%q,"note":"added, but Search Console still `+
			`requires the property to be verified before it returns data"}`, sub(d.GscSiteUrl)), nil

	case "delete_site":
		if err := needSite(); err != nil {
			return "", err
		}
		return googleCall(ctx, token, http.MethodDelete, gscAPI+"/sites/"+site(), nil)

	// ---- sitemaps ----
	case "list_sitemaps":
		if err := needSite(); err != nil {
			return "", err
		}
		return googleCall(ctx, token, http.MethodGet, gscAPI+"/sites/"+site()+"/sitemaps", nil)

	case "get_sitemap":
		if err := needSite(); err != nil {
			return "", err
		}
		if sub(d.GscFeedPath) == "" {
			return "", fmt.Errorf("get_sitemap needs the sitemap URL")
		}
		return googleCall(ctx, token, http.MethodGet,
			gscAPI+"/sites/"+site()+"/sitemaps/"+url.PathEscape(sub(d.GscFeedPath)), nil)

	case "submit_sitemap":
		if err := needSite(); err != nil {
			return "", err
		}
		if sub(d.GscFeedPath) == "" {
			return "", fmt.Errorf("submit_sitemap needs the sitemap URL, e.g. https://example.com/sitemap.xml")
		}
		if _, err := googleCall(ctx, token, http.MethodPut,
			gscAPI+"/sites/"+site()+"/sitemaps/"+url.PathEscape(sub(d.GscFeedPath)), nil); err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"ok":true,"sitemap":%q,"submitted":true}`, sub(d.GscFeedPath)), nil

	case "delete_sitemap":
		if err := needSite(); err != nil {
			return "", err
		}
		if sub(d.GscFeedPath) == "" {
			return "", fmt.Errorf("delete_sitemap needs the sitemap URL")
		}
		return googleCall(ctx, token, http.MethodDelete,
			gscAPI+"/sites/"+site()+"/sitemaps/"+url.PathEscape(sub(d.GscFeedPath)), nil)

	// ---- the useful one ----
	case "query_search_analytics":
		if err := needSite(); err != nil {
			return "", err
		}
		if sub(d.GscStartDate) == "" || sub(d.GscEndDate) == "" {
			return "", fmt.Errorf("query_search_analytics needs a start and end date as YYYY-MM-DD " +
				"— note Search Console data lags about two days behind")
		}
		body := map[string]any{
			"startDate": sub(d.GscStartDate),
			"endDate":   sub(d.GscEndDate),
			"rowLimit":  intOr(d.GscRowLimit, 100),
		}
		// Without dimensions the response is one aggregate row, which is rarely
		// what a workflow wants, so query, page and country are the usual picks.
		if dims := splitCSV(sub(d.GscDimensions)); len(dims) > 0 {
			body["dimensions"] = dims
		}
		if v := sub(d.GscSearchType); v != "" {
			body["type"] = v
		}
		if v := sub(d.GscDataState); v != "" {
			// "all" includes fresh but incomplete data; the default is final only.
			body["dataState"] = v
		}
		if n := d.GscStartRow; n > 0 {
			body["startRow"] = n
		}
		if f := strings.TrimSpace(sub(d.GscFilterExpression)); f != "" {
			group, err := gscFilterGroup(f)
			if err != nil {
				return "", err
			}
			body["dimensionFilterGroups"] = []any{group}
		}
		raw, err := googleCall(ctx, token, http.MethodPost,
			gscAPI+"/sites/"+site()+"/searchAnalytics/query", body)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 12000), nil

	case "inspect_url":
		// A different host and API version from everything else here.
		if err := needSite(); err != nil {
			return "", err
		}
		if sub(d.GscInspectionUrl) == "" {
			return "", fmt.Errorf("inspect_url needs the page URL to inspect")
		}
		body := map[string]any{
			"inspectionUrl": sub(d.GscInspectionUrl),
			"siteUrl":       sub(d.GscSiteUrl),
		}
		if v := sub(d.GscLanguageCode); v != "" {
			body["languageCode"] = v
		}
		raw, err := googleCall(ctx, token, http.MethodPost,
			gscInspectAPI+"/urlInspection/index:inspect", body)
		if err != nil {
			return "", err
		}
		return truncateStr(raw, 10000), nil

	case "":
		return "", fmt.Errorf("no Search Console operation selected")
	}
	return "", fmt.Errorf("unsupported Search Console operation: %s", d.IntegrationOp)
}

// gscFilterGroup parses a compact "dimension operator value" filter into the
// nested shape the API wants, so a node does not have to hand-write JSON for the
// common case of one filter.
func gscFilterGroup(expr string) (map[string]any, error) {
	filters := []any{}
	for _, line := range strings.Split(expr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 3 {
			return nil, fmt.Errorf(`each filter needs three parts — dimension, operator and value, ` +
				`e.g. "query contains pricing" or "country equals gbr"`)
		}
		filters = append(filters, map[string]any{
			"dimension":  parts[0],
			"operator":   parts[1],
			"expression": parts[2],
		})
	}
	if len(filters) == 0 {
		return nil, fmt.Errorf("no filters were parsed from that expression")
	}
	return map[string]any{"groupType": "and", "filters": filters}, nil
}
