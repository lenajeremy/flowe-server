package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Google Sheets API v4.

const gsheetsBase = "https://sheets.googleapis.com/v4/spreadsheets"

func runGoogleSheets(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(s string) string { return substituteTemplates(s, outputs) }
	id := sub(d.GSheetsSpreadsheetId)

	switch d.IntegrationOp {
	case "read_range":
		raw, err := googleCall(ctx, token, http.MethodGet,
			gsheetsBase+"/"+url.PathEscape(id)+"/values/"+url.PathEscape(sub(d.GSheetsRange)), nil)
		if err != nil {
			return "", err
		}
		return raw, nil

	case "append_row":
		row := sheetsCells(sub(d.GSheetsValues))
		body := map[string]any{"values": [][]string{row}}
		q := url.Values{}
		q.Set("valueInputOption", "USER_ENTERED")
		q.Set("insertDataOption", "INSERT_ROWS")
		if _, err := googleCall(ctx, token, http.MethodPost,
			gsheetsBase+"/"+url.PathEscape(id)+"/values/"+url.PathEscape(sub(d.GSheetsRange))+":append?"+q.Encode(), body); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": "appended", "cells": row})
		return string(b), nil

	case "update_range":
		row := sheetsCells(sub(d.GSheetsValues))
		body := map[string]any{"values": [][]string{row}}
		q := url.Values{}
		q.Set("valueInputOption", "USER_ENTERED")
		if _, err := googleCall(ctx, token, http.MethodPut,
			gsheetsBase+"/"+url.PathEscape(id)+"/values/"+url.PathEscape(sub(d.GSheetsRange))+"?"+q.Encode(), body); err != nil {
			return "", err
		}
		return `{"status":"updated"}`, nil

	case "create_spreadsheet":
		raw, err := googleCall(ctx, token, http.MethodPost, gsheetsBase,
			map[string]any{"properties": map[string]any{"title": sub(d.GSheetsTitle)}})
		if err != nil {
			return "", err
		}
		var res struct {
			SpreadsheetID  string `json:"spreadsheetId"`
			SpreadsheetURL string `json:"spreadsheetUrl"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		b, _ := json.Marshal(map[string]any{
			"status":        "created",
			"spreadsheetId": res.SpreadsheetID,
			"link":          res.SpreadsheetURL,
		})
		return string(b), nil

	default:
		return "", fmt.Errorf("unknown Google Sheets operation: %s", d.IntegrationOp)
	}
}

// sheetsCells splits a comma-separated value string into row cells. A JSON
// array (["a","b"]) is honoured as-is for values that themselves contain commas.
func sheetsCells(s string) []string {
	if trimmed := strings.TrimSpace(s); len(trimmed) > 0 && trimmed[0] == '[' {
		var arr []string
		if json.Unmarshal([]byte(trimmed), &arr) == nil {
			return arr
		}
	}
	return splitCSV(s)
}
