package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionFlagAndCommandAgree(t *testing.T) {
	run := func(args ...string) string {
		t.Helper()
		var buf bytes.Buffer
		rootCmd.SetOut(&buf)
		rootCmd.SetErr(&buf)
		rootCmd.SetArgs(args)
		t.Cleanup(func() {
			rootCmd.SetOut(nil)
			rootCmd.SetErr(nil)
			rootCmd.SetArgs(nil)
		})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("qmd %v: %v", args, err)
		}
		return buf.String()
	}

	flag := run("--version")
	command := run("version")
	if flag != command {
		t.Errorf("--version and version differ:\n--version: %q\nversion: %q", flag, command)
	}
	for _, want := range []string{"qmd version " + version, "Build: ", "Default embed model: "} {
		if !strings.Contains(flag, want) {
			t.Errorf("version output missing %q:\n%s", want, flag)
		}
	}
}
