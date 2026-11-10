package gateway

import "testing"

func TestBytesTrimRight(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"hello\n", "hello"},
		{"hello\r\n", "hello"},
		{"hello   \n", "hello"},
		{"hello", "hello"},
		{"", ""},
		{"\n", ""},
	}
	for _, tt := range tests {
		got := string(bytesTrimRight([]byte(tt.input)))
		if got != tt.expected {
			t.Errorf("bytesTrimRight(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		input    string
		maxLen   int
		expected string
	}{
		{"hello world", 5, "hello..."},
		{"hi", 10, "hi"},
		{"exactly10!", 10, "exactly10!"},
		{"", 5, ""},
	}
	for _, tt := range tests {
		got := truncate([]byte(tt.input), tt.maxLen)
		if got != tt.expected {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.expected)
		}
	}
}
