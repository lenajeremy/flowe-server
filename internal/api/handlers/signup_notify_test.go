package handlers

import "testing"

func TestSignupNotifyRecipients(t *testing.T) {
	t.Run("unset means the feature is off, not an error", func(t *testing.T) {
		t.Setenv("SIGNUP_NOTIFY_EMAIL", "")
		got, err := signupNotifyRecipients()
		if err != nil {
			t.Fatalf("an unset address list must not be an error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %v, want no recipients", got)
		}
	})

	t.Run("whitespace only is also off", func(t *testing.T) {
		t.Setenv("SIGNUP_NOTIFY_EMAIL", "   ")
		got, err := signupNotifyRecipients()
		if err != nil || len(got) != 0 {
			t.Fatalf("got %v, %v — want no recipients and no error", got, err)
		}
	})

	t.Run("a single address", func(t *testing.T) {
		t.Setenv("SIGNUP_NOTIFY_EMAIL", "founder@example.com")
		got, err := signupNotifyRecipients()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "founder@example.com" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("several, including display-name form", func(t *testing.T) {
		t.Setenv("SIGNUP_NOTIFY_EMAIL", "Ops <ops@example.com>, founder@example.com")
		got, err := signupNotifyRecipients()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"ops@example.com", "founder@example.com"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			// The display name is dropped — Resend wants bare addresses here.
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})

	t.Run("an unparseable value is an error, not a silent no-op", func(t *testing.T) {
		// Worth distinguishing: a typo'd address should be noisy, because the
		// symptom otherwise is notifications that never arrive and never explain.
		t.Setenv("SIGNUP_NOTIFY_EMAIL", "not-an-address")
		if _, err := signupNotifyRecipients(); err == nil {
			t.Fatal("expected an error for an unparseable address list")
		}
	})
}

func TestPlural(t *testing.T) {
	if plural(1) != "" {
		t.Fatal("1 account, not 1 accounts")
	}
	if plural(0) != "s" || plural(2) != "s" {
		t.Fatal("0 and 2 both take the plural")
	}
}
