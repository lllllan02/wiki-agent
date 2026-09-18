package agent

import "testing"

func TestNormalizeBaseURL(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"", ""},
		{"https://api.deepseek.com", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com/", "https://api.deepseek.com/v1"},
		{"https://api.deepseek.com/v1", "https://api.deepseek.com/v1"},
		{"http://localhost:3000/custom", "http://localhost:3000/custom"},
	} {
		got, err := normalizeBaseURL(test.input)
		if err != nil || got != test.want {
			t.Fatalf("normalizeBaseURL(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{"localhost:3000", "://bad"} {
		if _, err := normalizeBaseURL(input); err == nil {
			t.Fatalf("normalizeBaseURL(%q) should reject incomplete URL", input)
		}
	}
}
