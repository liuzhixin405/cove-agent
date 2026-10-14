package cost

import "testing"

// Bedrock model IDs are the first-party name with an "anthropic." prefix;
// pricing must find the exact entry through the substring match rather
// than fall back to unknownClaudePrice.
func TestPriceForBedrockPrefixedModelIDs(t *testing.T) {
	if got := priceFor("anthropic.claude-opus-5-5"); got != Prices["claude-opus-5-5"] {
		t.Fatalf("priceFor(anthropic.claude-opus-5-5) = %+v, want %+v", got, Prices["claude-opus-5-5"])
	}
	if got := priceFor("anthropic.claude-sonnet-5"); got != Prices["claude-sonnet-5"] {
		t.Fatalf("priceFor(anthropic.claude-sonnet-5) = %+v, want %+v", got, Prices["claude-sonnet-5"])
	}
}
