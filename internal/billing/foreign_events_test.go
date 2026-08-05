package billing

import (
	"context"
	"encoding/json"
	"testing"
)

// Events on our Stripe account that are not about us.
//
// A Stripe account carries traffic beyond our own checkout: Payment Links, a
// subscription set up by hand in the Dashboard, another product billing to the
// same account. Those arrive at our endpoint too.
//
// Returning an error for them looks conservative and is the opposite. Stripe reads
// a 5xx as "retry this", backs off for up to three days, and disables an endpoint
// that keeps failing — so an event that never concerned us ends up switching off
// the endpoint that real subscriptions depend on. Found by pointing a live test
// event at the deployed URL and getting a 500 back.
//
// The distinction these pin: an event we cannot attribute is not our business and
// must be acknowledged; an event we CAN attribute but fail to write is a genuine
// error and must still fail, because there a retry is exactly what we want.

func eventOf(t *testing.T, kind string, object any) stripeEvent {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ev := stripeEvent{ID: "evt_test", Type: kind}
	ev.Data.Object = raw
	return ev
}

func TestCheckoutSessionWithNoOrganizationIsAcknowledged(t *testing.T) {
	// A zero Gate is deliberate: this path must decide before it reaches the
	// database, so a nil db proves it never got that far.
	ev := eventOf(t, "checkout.session.completed", map[string]any{
		"id":       "cs_test_not_ours",
		"customer": "cus_stranger",
	})
	if err := (&Gate{}).onCheckoutCompleted(context.Background(), ev); err != nil {
		t.Fatalf("returned %v — Stripe will retry this for three days and then "+
			"disable the endpoint, taking real billing with it", err)
	}
}

func TestCheckoutSessionMetadataStillCountsAsOurs(t *testing.T) {
	// The ignore path must stay narrow. An org id in metadata rather than
	// client_reference_id is still our session, so it has to be acted on — and with
	// no database and no Stripe key behind it, acting on it can only fail. A nil
	// return here would mean the widened ignore had swallowed a real payment.
	// No Stripe key, so fetching the subscription this session paid for must fail —
	// which is the proof it tried. Cleared explicitly so a key in the developer's
	// environment cannot turn this into a live API call.
	t.Setenv("STRIPE_SECRET_KEY", "")
	ev := eventOf(t, "checkout.session.completed", map[string]any{
		"id":           "cs_test_ours",
		"subscription": "sub_test_ours",
		"metadata":     map[string]any{"organization_id": "11111111-1111-1111-1111-111111111111"},
	})
	if err := (&Gate{}).onCheckoutCompleted(context.Background(), ev); err == nil {
		t.Fatal("a session carrying our own organization_id was acknowledged without " +
			"being applied — a real upgrade would be dropped silently")
	}
}

func TestHandleWebhookIgnoresEventTypesWeDoNotSubscribeTo(t *testing.T) {
	// The endpoint is subscribed to five types, but Stripe can deliver more —
	// somebody widens the subscription in the Dashboard and every extra type starts
	// arriving. Unknown types must be acknowledged, not fail.
	for _, kind := range []string{"payment_intent.succeeded", "charge.refunded", "ping"} {
		payload, err := json.Marshal(map[string]any{
			"id": "evt_x", "type": kind,
			"data": map[string]any{"object": map[string]any{"id": "obj_x"}},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := (&Gate{}).HandleWebhook(context.Background(), payload); err != nil {
			t.Fatalf("%s returned %v, want nil", kind, err)
		}
	}
}

func TestUnparseableWebhookBodyIsStillAnError(t *testing.T) {
	// The ignore rule must not extend to a body we could not read at all. That is a
	// real fault and a retry may well succeed.
	if err := (&Gate{}).HandleWebhook(context.Background(), []byte("{not json")); err == nil {
		t.Fatal("an unreadable body was acknowledged as though it had been handled")
	}
}
