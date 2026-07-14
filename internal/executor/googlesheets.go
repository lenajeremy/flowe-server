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

	case "clear_range":
		if _, err := googleCall(ctx, token, http.MethodPost,
			gsheetsBase+"/"+url.PathEscape(id)+"/values/"+url.PathEscape(sub(d.GSheetsRange))+":clear", map[string]any{}); err != nil {
			return "", err
		}
		return `{"status":"cleared"}`, nil

	case "append_rows":
		var rows [][]string
		if err := json.Unmarshal([]byte(sub(d.GSheetsRows)), &rows); err != nil {
			return "", fmt.Errorf(`Google Sheets: rows must be a JSON array of arrays, e.g. [["a","b"],["c","d"]]: %w`, err)
		}
		body := map[string]any{"values": rows}
		q := url.Values{}
		q.Set("valueInputOption", "USER_ENTERED")
		q.Set("insertDataOption", "INSERT_ROWS")
		rng := firstNonEmpty(sub(d.GSheetsRange), "A1")
		if _, err := googleCall(ctx, token, http.MethodPost,
			gsheetsBase+"/"+url.PathEscape(id)+"/values/"+url.PathEscape(rng)+":append?"+q.Encode(), body); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": "appended", "rows": len(rows)})
		return string(b), nil

	case "list_sheets":
		raw, err := googleCall(ctx, token, http.MethodGet,
			gsheetsBase+"/"+url.PathEscape(id)+"?fields=sheets(properties(sheetId,title,index,gridProperties(rowCount,columnCount)))", nil)
		if err != nil {
			return "", err
		}
		var res struct {
			Sheets []struct {
				Properties struct {
					SheetID int    `json:"sheetId"`
					Title   string `json:"title"`
					Index   int    `json:"index"`
					Grid    struct {
						Rows int `json:"rowCount"`
						Cols int `json:"columnCount"`
					} `json:"gridProperties"`
				} `json:"properties"`
			} `json:"sheets"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		out := make([]map[string]any, 0, len(res.Sheets))
		for _, sh := range res.Sheets {
			out = append(out, map[string]any{
				"sheetId": sh.Properties.SheetID, "title": sh.Properties.Title,
				"rows": sh.Properties.Grid.Rows, "columns": sh.Properties.Grid.Cols,
			})
		}
		b, _ := json.Marshal(out)
		return string(b), nil

	case "add_sheet":
		raw, err := googleCall(ctx, token, http.MethodPost, gsheetsBase+"/"+url.PathEscape(id)+":batchUpdate",
			map[string]any{"requests": []map[string]any{
				{"addSheet": map[string]any{"properties": map[string]any{"title": sub(d.GSheetsSheetTitle)}}},
			}})
		if err != nil {
			return "", err
		}
		var res struct {
			Replies []struct {
				AddSheet struct {
					Properties struct {
						SheetID int `json:"sheetId"`
					} `json:"properties"`
				} `json:"addSheet"`
			} `json:"replies"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		sheetID := 0
		if len(res.Replies) > 0 {
			sheetID = res.Replies[0].AddSheet.Properties.SheetID
		}
		b, _ := json.Marshal(map[string]any{"status": "added", "sheetId": sheetID, "title": sub(d.GSheetsSheetTitle)})
		return string(b), nil

	case "delete_sheet":
		sheetID, err := gsheetsSheetID(ctx, token, id, sub(d.GSheetsSheetTitle))
		if err != nil {
			return "", err
		}
		if _, err := googleCall(ctx, token, http.MethodPost, gsheetsBase+"/"+url.PathEscape(id)+":batchUpdate",
			map[string]any{"requests": []map[string]any{
				{"deleteSheet": map[string]any{"sheetId": sheetID}},
			}}); err != nil {
			return "", err
		}
		return `{"status":"deleted"}`, nil

	case "delete_rows":
		if d.GSheetsStartRow < 1 || d.GSheetsEndRow < d.GSheetsStartRow {
			return "", fmt.Errorf("Google Sheets: start/end rows must be 1-based with end ≥ start")
		}
		sheetID, err := gsheetsSheetID(ctx, token, id, sub(d.GSheetsSheetTitle))
		if err != nil {
			return "", err
		}
		if _, err := googleCall(ctx, token, http.MethodPost, gsheetsBase+"/"+url.PathEscape(id)+":batchUpdate",
			map[string]any{"requests": []map[string]any{
				{"deleteDimension": map[string]any{"range": map[string]any{
					"sheetId":    sheetID,
					"dimension":  "ROWS",
					"startIndex": d.GSheetsStartRow - 1, // API is 0-based, end exclusive
					"endIndex":   d.GSheetsEndRow,
				}}},
			}}); err != nil {
			return "", err
		}
		b, _ := json.Marshal(map[string]any{"status": "deleted", "rows": d.GSheetsEndRow - d.GSheetsStartRow + 1})
		return string(b), nil

	case "find_replace":
		raw, err := googleCall(ctx, token, http.MethodPost, gsheetsBase+"/"+url.PathEscape(id)+":batchUpdate",
			map[string]any{"requests": []map[string]any{
				{"findReplace": map[string]any{
					"find":        sub(d.GSheetsFind),
					"replacement": sub(d.GSheetsReplace),
					"allSheets":   true,
					"matchCase":   true,
				}},
			}})
		if err != nil {
			return "", err
		}
		var res struct {
			Replies []struct {
				FindReplace struct {
					OccurrencesChanged int `json:"occurrencesChanged"`
				} `json:"findReplace"`
			} `json:"replies"`
		}
		_ = json.Unmarshal([]byte(raw), &res)
		changed := 0
		if len(res.Replies) > 0 {
			changed = res.Replies[0].FindReplace.OccurrencesChanged
		}
		b, _ := json.Marshal(map[string]any{"status": "replaced", "occurrences": changed})
		return string(b), nil
	}
}

// gsheetsSheetID resolves a tab title to its numeric sheetId.
func gsheetsSheetID(ctx context.Context, token, spreadsheetID, title string) (int, error) {
	raw, err := googleCall(ctx, token, http.MethodGet,
		gsheetsBase+"/"+url.PathEscape(spreadsheetID)+"?fields=sheets(properties(sheetId,title))", nil)
	if err != nil {
		return 0, err
	}
	var res struct {
		Sheets []struct {
			Properties struct {
				SheetID int    `json:"sheetId"`
				Title   string `json:"title"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	_ = json.Unmarshal([]byte(raw), &res)
	for _, sh := range res.Sheets {
		if sh.Properties.Title == title {
			return sh.Properties.SheetID, nil
		}
	}
	return 0, fmt.Errorf("Google Sheets: no sheet named %q", title)
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
