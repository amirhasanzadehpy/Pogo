package lsp

import (
	"slices"
	"testing"
)

func TestParseSignatureAndActiveParameter(t *testing.T) {
	parsed, ok := parseSignature("(status: str, limit: int = 10, /, *values: tuple[int, str], include_hidden: bool = False, **options) -> QuerySet")
	if !ok {
		t.Fatal("parseSignature() = false")
	}
	labels := make([]string, len(parsed.parameters))
	for index, parameter := range parsed.parameters {
		labels[index] = parameter.label
	}
	want := []string{"status: str", "limit: int = 10", "*values: tuple[int, str]", "include_hidden: bool = False", "**options"}
	if !slices.Equal(labels, want) {
		t.Fatalf("parameter labels = %v, want %v", labels, want)
	}
	tests := []struct {
		pos     int
		keyword string
		want    int
	}{
		{0, "", 0},
		{1, "", 1},
		{2, "", 2},
		{20, "", 2},
		{0, "include_hidden", 3},
		{0, "values", 4},
		{0, "unknown", 4},
	}
	for _, test := range tests {
		if got, exists := parsed.activeParameter(test.pos, test.keyword); !exists || got != test.want {
			t.Errorf("activeParameter(%d, %q) = %d, %v; want %d", test.pos, test.keyword, got, exists, test.want)
		}
	}
}

func TestParseSignatureRejectsMalformedInput(t *testing.T) {
	for _, signature := range []string{"", "value", "(value", "(value: tuple[int, str]) extra )", "(value: tuple[int, str)", "(value: tuple[int))"} {
		if parsed, ok := parseSignature(signature); ok {
			t.Errorf("parseSignature(%q) = %#v, true", signature, parsed)
		}
	}
}
