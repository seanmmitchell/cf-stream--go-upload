package main

import "testing"

func TestTokenArgs(t *testing.T) {
	tests := []struct {
		args           []string
		asFlag, inline bool
	}{
		{nil, false, false},
		{[]string{"--file", "a.mp4", "--acctid", "b"}, false, false},
		{[]string{"--token", "x"}, true, false},
		{[]string{"--apitoken", "x"}, true, false},
		// transporter drops the first two characters, whatever they are.
		{[]string{"-xtoken", "x"}, true, false},
		{[]string{"-token", "x"}, false, false},
		{[]string{"--tokens", "x"}, false, false},
		{[]string{"--token=x"}, false, true},
		{[]string{"-xapitoken=x"}, false, true},
		{[]string{"--file=a.mp4"}, false, false},
		// The value after a token flag is the token, even if it looks like a flag.
		{[]string{"--token", "--token=x"}, true, false},
		{[]string{"-", "--"}, false, false},
	}
	for _, tt := range tests {
		asFlag, inline := tokenArgs(tt.args)
		if asFlag != tt.asFlag || inline != tt.inline {
			t.Errorf("tokenArgs(%q) = %v, %v, want %v, %v", tt.args, asFlag, inline, tt.asFlag, tt.inline)
		}
	}
}
