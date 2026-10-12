package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/seanmmitchell/transporter"
)

func TestCheckArgs(t *testing.T) {
	names := []string{"acctid", "apitoken", "token", "file", "chunksize"}
	tests := []struct {
		args        []string
		tokenAsFlag bool
		ok          bool
	}{
		{nil, false, true},
		{[]string{"--file", "a.mp4", "--acctid", "b", "--chunksize", "20"}, false, true},
		{[]string{"--token", "x"}, true, true},
		{[]string{"--acctid", "b", "--apitoken", "x"}, true, true},
		// transporter drops the first two characters, whatever they are.
		{[]string{"-xtoken", "x"}, true, true},
		// A value is never read as a flag, even if it looks like one.
		{[]string{"--token", "-x"}, true, true},
		{[]string{"--file", "--token=a.mp4"}, false, true},
		// Rejected: transporter would print these, and one may be the token.
		{[]string{"-token", "S3CRET"}, false, false},
		{[]string{"--token=S3CRET"}, false, false},
		{[]string{"--TOKEN", "S3CRET"}, false, false},
		{[]string{"S3CRET"}, false, false},
		{[]string{"xxtoken", "S3CRET"}, false, false},
		{[]string{"--file", "a.mp4", "S3CRET"}, false, false},
		{[]string{"-", "S3CRET"}, false, false},
		// Rejected: a flag without its value, which transporter ignores.
		{[]string{"--token"}, false, false},
		{[]string{"--acctid", "b", "--file"}, false, false},
	}
	for _, tt := range tests {
		tokenAsFlag, err := checkArgs(tt.args, names)
		if tokenAsFlag != tt.tokenAsFlag || (err == nil) != tt.ok {
			t.Errorf("checkArgs(%q) = %v, %v, want %v and ok %v", tt.args, tokenAsFlag, err, tt.tokenAsFlag, tt.ok)
		}
		if err != nil && strings.Contains(err.Error(), "S3CRET") {
			t.Errorf("checkArgs(%q) error prints an argument: %v", tt.args, err)
		}
	}
}

func TestFlagNames(t *testing.T) {
	p := transporter.Pattern{Sequences: map[string]transporter.PatternSequence{
		"apitoken": {CLIFlags: []string{"token"}, ENVVars: []string{"key"}},
		"file":     {CLIFlags: []string{"file"}},
	}}
	got := flagNames(p)
	slices.Sort(got)
	if want := []string{"apitoken", "file", "file", "key", "token"}; !slices.Equal(got, want) {
		t.Errorf("flagNames = %q, want %q", got, want)
	}
}
