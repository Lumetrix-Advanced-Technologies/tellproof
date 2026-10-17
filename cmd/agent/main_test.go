package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

var (
	example = filepath.Join("..", "..", "agent.example.json")
	sample  = filepath.Join("..", "..", "concierge", "testdata", "otlp-sample.jsonl")
)

func getenv(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestDryRunReplay(t *testing.T) {
	var out, errb bytes.Buffer
	env := getenv("TELLPROOF_SALT_SECRET", "a-salt-secret-0123456", "TELLPROOF_DATA_DIR", filepath.Join(t.TempDir(), "d"))
	code := run(context.Background(), []string{"--config", example, "--dry-run", "--replay", sample}, env, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	dec := json.NewDecoder(&out)
	n := 0
	for dec.More() {
		var item map[string]json.RawMessage
		if err := dec.Decode(&item); err != nil || item["envelope"] == nil || item["payload"] == nil {
			t.Fatalf("item %d: %v", n, err)
		}
		n++
	}
	if n != 2 {
		t.Errorf("printed %d batches; want 2", n)
	}
}

func TestUsageErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":        {"--frobnicate"},
		"stray argument":      {"run", "extra"},
		"token on run":        {"--token", "x"},
		"dry-run on enrol":    {"enrol", "--dry-run"},
		"enrol without token": {"enrol", "--config", example},
		"missing config":      {"--config", "nope.json"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := run(context.Background(), args, getenv(), &out, &errb); code != 2 {
				t.Errorf("exit %d; want 2 (%s)", code, errb.String())
			}
		})
	}
}

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if code := run(context.Background(), []string{"version"}, getenv(), &out, &out); code != 0 || !strings.HasPrefix(out.String(), "tellproof-agent ") {
		t.Errorf("version: exit %d %q", code, out.String())
	}
}
