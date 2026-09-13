package beadmeta

import (
	"strings"
	"testing"
)

func TestValidateMetadataValue(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string // substring; empty means accepted
	}{
		{
			name:    "routed_to empty is the wipe signature",
			key:     RoutedToMetadataKey,
			value:   "",
			wantErr: "unsetting the key",
		},
		{
			name:  "routed_to non-empty target is legal",
			key:   RoutedToMetadataKey,
			value: "voxist.platform-architect",
		},
		{
			name:  "routed_to pool-alias target is legal",
			key:   RoutedToMetadataKey,
			value: "voxist__platform-architect-2-pool",
		},
		{
			name:    "awaiting_human prose is not a predicate",
			key:     AwaitingHumanMetadataKey,
			value:   "integration-test-not-in-CI-followup",
			wantErr: "gc.human_ask",
		},
		{
			name:    "awaiting_human identity is not a predicate",
			key:     AwaitingHumanMetadataKey,
			value:   "karel@voxist.com",
			wantErr: "gc.human_owner",
		},
		{
			name:    "awaiting_human false is negation-by-value, not absence",
			key:     AwaitingHumanMetadataKey,
			value:   "false",
			wantErr: "key absence",
		},
		{
			name:  "awaiting_human literal true is the only affirmative",
			key:   AwaitingHumanMetadataKey,
			value: "true",
		},
		{
			name:  "unconventioned keys stay open-world, including empty",
			key:   "gc.human_ask",
			value: "",
		},
		{
			name:  "unknown keys stay open-world",
			key:   "custom.pack.key",
			value: "anything",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMetadataValue(tt.key, tt.value)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateMetadataValue(%q, %q) = %v, want nil", tt.key, tt.value, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateMetadataValue(%q, %q) = nil, want error containing %q", tt.key, tt.value, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateMetadataValue(%q, %q) = %q, want substring %q", tt.key, tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestValidateMetadataValuesReturnsFirstViolationInSortedOrder(t *testing.T) {
	kvs := map[string]string{
		AwaitingHumanMetadataKey: "1",                // sorts before gc.routed_to
		RoutedToMetadataKey:      "",                 // also invalid
		"gc.human_ask":           "unrelated, legal", // open-world
	}
	err := ValidateMetadataValues(kvs)
	if err == nil {
		t.Fatal("ValidateMetadataValues = nil, want the awaiting_human violation first")
	}
	if !strings.Contains(err.Error(), "gc.awaiting_human") {
		t.Fatalf("ValidateMetadataValues = %q, want the sorted-first awaiting_human violation", err)
	}
}

func TestValidateMetadataValuesAcceptsCleanBatch(t *testing.T) {
	kvs := map[string]string{
		RoutedToMetadataKey:      "voxist.platform-architect",
		AwaitingHumanMetadataKey: "true",
		"gc.human_ask":           "",
	}
	if err := ValidateMetadataValues(kvs); err != nil {
		t.Fatalf("ValidateMetadataValues = %v, want nil", err)
	}
}
