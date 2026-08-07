package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type mondayAsanaRoundTripFunc func(*http.Request) (*http.Response, error)

func (f mondayAsanaRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestMondayUpdateItemUsesGraphQLVariablesAndRawToken(t *testing.T) {
	oldURL := mondayAPIURL
	oldClient := integrationHTTP
	defer func() { mondayAPIURL, integrationHTTP = oldURL, oldClient }()
	mondayAPIURL = "https://monday.test/v2"
	integrationHTTP = &http.Client{Transport: mondayAsanaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "monday-token" || r.Header.Get("API-Version") != "2026-04" {
			t.Errorf("headers = %#v", r.Header)
		}
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request.Variables["board"] != "10" || request.Variables["item"] != "20" {
			t.Errorf("variables = %#v", request.Variables)
		}
		if request.Variables["values"] != `{"status":{"label":"Done"}}` {
			t.Errorf("column values = %#v", request.Variables["values"])
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"data":{"change_multiple_column_values":{"id":"20","name":"Task"}}}`))}, nil
	})}

	result, err := runMonday(context.Background(), "monday-token", FlowNodeData{
		IntegrationOp: "update_item", MondayBoardId: "10", MondayItemId: "20",
		MondayColumnValues: `{"status":{"label":"Done"}}`,
	}, nil)
	if err != nil || result == "" {
		t.Fatalf("runMonday = %q, %v", result, err)
	}
}

func TestAsanaUpdateCanExplicitlyMarkTaskIncomplete(t *testing.T) {
	oldURL := asanaAPIURL
	oldClient := integrationHTTP
	defer func() { asanaAPIURL, integrationHTTP = oldURL, oldClient }()
	asanaAPIURL = "https://asana.test/api/1.0"
	integrationHTTP = &http.Client{Transport: mondayAsanaRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/1.0/tasks/123" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer asana-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var request struct {
			Data map[string]any `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if completed, ok := request.Data["completed"].(bool); !ok || completed {
			t.Errorf("payload = %#v", request.Data)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"data":{"gid":"123","completed":false}}`))}, nil
	})}

	result, err := runAsana(context.Background(), "asana-token", FlowNodeData{
		IntegrationOp: "update_task", AsanaTaskId: "123", AsanaCompleted: "false",
	}, nil)
	if err != nil || result == "" {
		t.Fatalf("runAsana = %q, %v", result, err)
	}
}
