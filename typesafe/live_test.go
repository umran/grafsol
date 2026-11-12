package typesafe

import (
	"os"
	"testing"
)

// TestLiveSystemOne asks the real API one small question of each kind, to
// confirm that the request and the answers have the shape this client
// expects. It is skipped unless GRAFSOL_LIVE_TYPESAFE is set, and needs
// TYPESAFE_API_KEY. It costs a fraction of a cent.
//
//	GRAFSOL_LIVE_TYPESAFE=1 TYPESAFE_API_KEY=... go test ./typesafe/ -run TestLiveSystemOne -v
func TestLiveSystemOne(t *testing.T) {
	if os.Getenv("GRAFSOL_LIVE_TYPESAFE") == "" {
		t.Skip("GRAFSOL_LIVE_TYPESAFE is not set")
	}
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		t.Fatal("TYPESAFE_API_KEY is not set")
	}
	client := New(Config{APIKey: key, UserAgent: "go-test-client"})

	response, err := client.SystemOne(t.Context(), Request{
		State: map[string]any{"message": "I was charged twice for my subscription this month. Please refund the extra charge today."},
		Questions: map[string]Question{
			"billing": Noul("Is `message` about billing?", "It concerns a charge, an invoice or a refund.", "It concerns something else."),
			"weather": Noul("Is `message` about the weather?", "", ""),
			"urgency": Score("How urgent is `message`?", "Can wait", "Needs attention this week", "Needs attention today"),
			"tone":    Choice("What is the tone of `message`?", map[string]string{"angry": "Upset or hostile", "calm": "Neutral or polite", "excited": "Enthusiastic"}),
		},
	})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	t.Logf("answered by %s for %d input tokens", response.Model, response.Usage.InputTokens)
	for name, answer := range response.Answers {
		t.Logf("  %s: %+v", name, answer)
	}

	if response.Model == "" || response.Usage.InputTokens <= 0 {
		t.Errorf("the response names model %q and %d input tokens", response.Model, response.Usage.InputTokens)
	}
	if got := response.Answers["billing"]; got.Type != "noul" || got.Noul < 0.7 {
		t.Errorf("a message about a double charge is about billing with probability %v", got.Noul)
	}
	if got := response.Answers["weather"]; got.Type != "noul" || got.Noul > 0.3 {
		t.Errorf("a message about a double charge is about the weather with probability %v", got.Noul)
	}
	if got := response.Answers["urgency"]; got.Type != "score" || got.Score < 0 || got.Score > 2 || len(got.Probabilities) != 3 {
		t.Errorf("the score came back as %+v", got)
	}
	if got := response.Answers["tone"]; got.Type != "choice" || got.Probabilities[got.Choice] == 0 {
		t.Errorf("the choice came back as %+v", got)
	}
}
