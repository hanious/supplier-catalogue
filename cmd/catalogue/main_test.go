package main

import "testing"

func TestRunRequiresDynamoDBEndpoint(t *testing.T) {
	t.Setenv("DYNAMODB_ENDPOINT", "")
	for _, mode := range []string{"init", "api", "supplier", "worker"} {
		t.Run(mode, func(t *testing.T) {
			if err := run([]string{mode}); err == nil {
				t.Errorf("run(%q) without DYNAMODB_ENDPOINT succeeded, want startup failure", mode)
			}
		})
	}
}

func TestRunRejectsUnsupportedCommands(t *testing.T) {
	t.Setenv("DYNAMODB_ENDPOINT", "http://127.0.0.1:1")
	for _, args := range [][]string{nil, {"dispatcher"}, {"unknown"}, {"api", "extra"}} {
		if err := run(args); err == nil {
			t.Errorf("run(%q) succeeded, want invalid command failure", args)
		}
	}
}
