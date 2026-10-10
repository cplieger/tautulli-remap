package remap

import (
	"encoding/json"
	"testing"
)

func TestFlexIntUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		json string
		want flexInt
	}{
		{"float", "42.0", 42},
		{"string", `"123"`, 123},
		{"empty string", `""`, 0},
		{"null", "null", 0},
		{"invalid string", `"abc"`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f flexInt
			if err := json.Unmarshal([]byte(tt.json), &f); err != nil {
				t.Fatalf("unmarshal error: %v", err)
			}
			if f != tt.want {
				t.Errorf("got %d, want %d", f, tt.want)
			}
		})
	}
}

func TestMediaType(t *testing.T) {
	if Movie.String() != "movie" {
		t.Errorf("Movie.String() = %q", Movie.String())
	}
}

// TestParseMediaType pins the string-to-MediaType mapping, including that an
// unrecognized type yields the empty MediaType (which drops such items from
// the index downstream).
func TestParseMediaType(t *testing.T) {
	tests := []struct {
		in   string
		want MediaType
	}{
		{"movie", Movie},
		{"show", Show},
		{"episode", episode},
		{"artist", ""},
		{"track", ""},
		{"", ""},
		{"MOVIE", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseMediaType(tt.in); got != tt.want {
				t.Errorf("parseMediaType(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRatingKeyIsValid(t *testing.T) {
	tests := []struct {
		input RatingKey
		want  bool
	}{
		{"42", true},
		{"0", true},
		{"", false},
		{"-1", false},
		{"abc", false},
	}
	for _, tt := range tests {
		if got := tt.input.IsValid(); got != tt.want {
			t.Errorf("RatingKey(%q).IsValid() = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestFlexIntUnmarshalJSON_nonNumericTypesCoerceToZero(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"bool true", "true"},
		{"bool false", "false"},
		{"array", "[1,2,3]"},
		{"object", `{"a":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := flexInt(99)
			if err := f.UnmarshalJSON([]byte(tt.json)); err != nil {
				t.Fatalf("UnmarshalJSON(%s) unexpected error: %v", tt.json, err)
			}
			if f != 0 {
				t.Errorf("UnmarshalJSON(%s) = %d, want 0 (non-numeric JSON coerces to zero)", tt.json, f)
			}
		})
	}
}

func TestFlexIntUnmarshalJSON_malformedReturnsError(t *testing.T) {
	var f flexInt
	if err := f.UnmarshalJSON([]byte("{")); err == nil {
		t.Error("UnmarshalJSON of malformed JSON should return an error")
	}
}
