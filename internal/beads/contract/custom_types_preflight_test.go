package contract

import (
	"errors"
	"reflect"
	"testing"
)

func TestEvaluateCustomTypesRegistered(t *testing.T) {
	tests := []struct {
		name          string
		required      []string
		registered    []string
		wantSatisfied bool
		wantMissing   []string
	}{
		{
			name:          "all required present",
			required:      []string{"session", "step", "convergence"},
			registered:    []string{"session", "step", "convergence", "molecule"},
			wantSatisfied: true,
			wantMissing:   nil,
		},
		{
			name:          "exact match",
			required:      []string{"session", "step"},
			registered:    []string{"session", "step"},
			wantSatisfied: true,
			wantMissing:   nil,
		},
		{
			name:          "one missing",
			required:      []string{"session", "step", "convergence"},
			registered:    []string{"session", "step"},
			wantSatisfied: false,
			wantMissing:   []string{"convergence"},
		},
		{
			name:          "several missing sorted",
			required:      []string{"session", "step", "convergence", "spec"},
			registered:    []string{"session"},
			wantSatisfied: false,
			wantMissing:   []string{"convergence", "spec", "step"},
		},
		{
			name:          "empty registered set",
			required:      []string{"session", "step"},
			registered:    nil,
			wantSatisfied: false,
			wantMissing:   []string{"session", "step"},
		},
		{
			name:          "empty required set is trivially satisfied",
			required:      nil,
			registered:    []string{"session"},
			wantSatisfied: true,
			wantMissing:   nil,
		},
		{
			name:          "whitespace and empties normalized",
			required:      []string{" session ", "", "step"},
			registered:    []string{"session", "  ", "step"},
			wantSatisfied: true,
			wantMissing:   nil,
		},
		{
			name:          "duplicate registered entries deduped",
			required:      []string{"session"},
			registered:    []string{"session", "session", "session"},
			wantSatisfied: true,
			wantMissing:   nil,
		},
		{
			name:          "duplicate required entries deduped in missing",
			required:      []string{"step", "step"},
			registered:    nil,
			wantSatisfied: false,
			wantMissing:   []string{"step"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateCustomTypesRegistered(tt.required, tt.registered)
			if got.Satisfied != tt.wantSatisfied {
				t.Errorf("Satisfied = %v, want %v", got.Satisfied, tt.wantSatisfied)
			}
			if !reflect.DeepEqual(got.Missing, tt.wantMissing) {
				t.Errorf("Missing = %v, want %v", got.Missing, tt.wantMissing)
			}
		})
	}
}

func TestGateCustomTypesRegistered_SatisfiedReturnsNil(t *testing.T) {
	err := GateCustomTypesRegistered(
		[]string{"session", "step"},
		[]string{"session", "step", "molecule"},
	)
	if err != nil {
		t.Fatalf("GateCustomTypesRegistered() = %v, want nil", err)
	}
}

func TestGateCustomTypesRegistered_MissingRefusesWithTypedError(t *testing.T) {
	err := GateCustomTypesRegistered(
		[]string{"session", "step", "convergence"},
		[]string{"session"},
	)
	if err == nil {
		t.Fatal("GateCustomTypesRegistered() = nil, want error")
	}
	if !errors.Is(err, ErrCustomTypesNotRegistered) {
		t.Errorf("error = %v, want errors.Is ErrCustomTypesNotRegistered", err)
	}
	// The missing types must be named in the refusal so it self-diagnoses.
	for _, want := range []string{"convergence", "step"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q does not name missing type %q", err.Error(), want)
		}
	}
}

func TestGateCustomTypesRegistered_EmptyRequiredIsSatisfied(t *testing.T) {
	if err := GateCustomTypesRegistered(nil, nil); err != nil {
		t.Fatalf("GateCustomTypesRegistered(nil, nil) = %v, want nil", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
