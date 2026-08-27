package codingagent

import (
	"context"
	"strings"
	"testing"
)

func TestMintToolTokenRequiresSecureParsedCallbackURL(t *testing.T) {
	store, db := testStore(t)
	connectedCredential(t, db)
	job, _, err := store.Submit(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{db: db}

	t.Setenv("PUBLIC_BASE_URL", "http://example.com/localhost")
	if _, _, err := service.mintToolToken(context.Background(), job); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("insecure callback error = %v, want HTTPS rejection", err)
	}

	t.Setenv("PUBLIC_BASE_URL", "https://fernary.example")
	endpoint, token, err := service.mintToolToken(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://fernary.example"+ToolCallbackPath || token == "" {
		t.Fatalf("endpoint=%q tokenPresent=%t", endpoint, token != "")
	}
}
