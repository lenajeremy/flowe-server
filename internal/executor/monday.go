package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var mondayAPIURL = "https://api.monday.com/v2"

func mondayCall(ctx context.Context, token, query string, variables map[string]any) (string, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return "", fmt.Errorf("encode monday.com request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mondayAPIURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("API-Version", "2026-04")
	resp, err := integrationHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("monday.com request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("monday.com API returned %d: %s", resp.StatusCode, truncateStr(string(raw), 300))
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("monday.com returned unreadable JSON: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return "", fmt.Errorf("monday.com API error: %s", envelope.Errors[0].Message)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return "", fmt.Errorf("monday.com API returned no data")
	}
	return string(envelope.Data), nil
}

func runMonday(ctx context.Context, token string, d FlowNodeData, outputs map[string]string) (string, error) {
	sub := func(value string) string { return substituteTemplates(value, outputs) }
	need := func(label, value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("this operation needs %s", label)
		}
		return nil
	}
	board := sub(d.MondayBoardId)
	item := sub(d.MondayItemId)
	group := sub(d.MondayGroupId)
	limit := intOr(d.MondayLimit, 25)
	if limit < 1 {
		limit = 25
	}
	if limit > 100 {
		limit = 100
	}

	switch d.IntegrationOp {
	case "list_boards":
		return mondayCall(ctx, token, `query ($limit: Int!) { boards(limit: $limit, state: active) { id name description workspace_id board_kind state url } }`, map[string]any{"limit": limit})

	case "get_board":
		if err := need("a board ID", board); err != nil {
			return "", err
		}
		return mondayCall(ctx, token, `query ($ids: [ID!]) { boards(ids: $ids) { id name description workspace_id board_kind state url groups { id title } columns { id title type } } }`, map[string]any{"ids": []string{board}})

	case "list_items":
		if err := need("a board ID", board); err != nil {
			return "", err
		}
		return mondayCall(ctx, token, `query ($ids: [ID!], $limit: Int!, $cursor: String) { boards(ids: $ids) { id items_page(limit: $limit, cursor: $cursor) { cursor items { id name state group { id title } column_values { id text value } } } } }`, map[string]any{"ids": []string{board}, "limit": limit, "cursor": nullableString(sub(d.MondayCursor))})

	case "get_item":
		if err := need("an item ID", item); err != nil {
			return "", err
		}
		return mondayCall(ctx, token, `query ($ids: [ID!]) { items(ids: $ids) { id name state url board { id name } group { id title } column_values { id text value } } }`, map[string]any{"ids": []string{item}})

	case "create_item":
		if err := need("a board ID", board); err != nil {
			return "", err
		}
		name := sub(d.MondayItemName)
		if err := need("an item name", name); err != nil {
			return "", err
		}
		columnValues, err := mondayJSONObject(sub(d.MondayColumnValues), false)
		if err != nil {
			return "", err
		}
		return mondayCall(ctx, token, `mutation ($board: ID!, $group: String, $name: String!, $values: JSON) { create_item(board_id: $board, group_id: $group, item_name: $name, column_values: $values) { id name url } }`, map[string]any{"board": board, "group": nullableString(group), "name": name, "values": columnValues})

	case "update_item":
		if err := need("a board ID", board); err != nil {
			return "", err
		}
		if err := need("an item ID", item); err != nil {
			return "", err
		}
		columnValues, err := mondayJSONObject(sub(d.MondayColumnValues), true)
		if err != nil {
			return "", err
		}
		return mondayCall(ctx, token, `mutation ($board: ID!, $item: ID!, $values: JSON!) { change_multiple_column_values(board_id: $board, item_id: $item, column_values: $values) { id name url } }`, map[string]any{"board": board, "item": item, "values": columnValues})

	case "move_item_to_group":
		if err := need("an item ID", item); err != nil {
			return "", err
		}
		if err := need("a group ID", group); err != nil {
			return "", err
		}
		return mondayCall(ctx, token, `mutation ($item: ID!, $group: String!) { move_item_to_group(item_id: $item, group_id: $group) { id name } }`, map[string]any{"item": item, "group": group})

	case "archive_item":
		if err := need("an item ID", item); err != nil {
			return "", err
		}
		return mondayCall(ctx, token, `mutation ($item: ID!) { archive_item(item_id: $item) { id state } }`, map[string]any{"item": item})

	case "delete_item":
		if err := need("an item ID", item); err != nil {
			return "", err
		}
		return mondayCall(ctx, token, `mutation ($item: ID!) { delete_item(item_id: $item) { id } }`, map[string]any{"item": item})

	case "create_update":
		if err := need("an item ID", item); err != nil {
			return "", err
		}
		body := sub(d.MondayUpdateBody)
		if err := need("an update body", body); err != nil {
			return "", err
		}
		return mondayCall(ctx, token, `mutation ($item: ID!, $body: String!) { create_update(item_id: $item, body: $body) { id body creator { id name } created_at } }`, map[string]any{"item": item, "body": body})

	case "list_updates":
		if err := need("an item ID", item); err != nil {
			return "", err
		}
		return mondayCall(ctx, token, `query ($ids: [ID!], $limit: Int!) { items(ids: $ids) { id updates(limit: $limit) { id body text_body created_at creator { id name } } } }`, map[string]any{"ids": []string{item}, "limit": limit})

	case "list_users":
		return mondayCall(ctx, token, `query ($limit: Int!) { users(limit: $limit) { id name email enabled } }`, map[string]any{"limit": limit})

	default:
		return "", fmt.Errorf("unknown monday.com operation %q", d.IntegrationOp)
	}
}

func mondayJSONObject(value string, required bool) (any, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		if required {
			return nil, fmt.Errorf("this operation needs column values as a JSON object")
		}
		return nil, nil
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(value), &object); err != nil {
		return nil, fmt.Errorf("mondayColumnValues must be a JSON object: %w", err)
	}
	if required && len(object) == 0 {
		return nil, fmt.Errorf("mondayColumnValues must contain at least one column")
	}
	// monday.com's JSON scalar expects a serialized JSON object, not a GraphQL
	// input object. Passing the string as a variable also preserves templates.
	encoded, _ := json.Marshal(object)
	return string(encoded), nil
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
