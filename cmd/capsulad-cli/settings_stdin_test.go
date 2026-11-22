package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildAppSettingChangesReadsRawStringFromStdin(t *testing.T) {
	const secret = `{"token": "not-json"}`
	changes, err := buildAppSettingChanges([]string{"marusia.session@-"}, func() (string, error) {
		return secret, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Key != "marusia.session" {
		t.Fatalf("changes = %+v", changes)
	}
	if changes[0].ValueJson != `"{\"token\": \"not-json\"}"` {
		t.Fatalf("value JSON = %s; want a quoted raw string", changes[0].ValueJson)
	}
	var decoded string
	if err := json.Unmarshal([]byte(changes[0].ValueJson), &decoded); err != nil || decoded != secret {
		t.Fatalf("decoded = %q, %v", decoded, err)
	}
}

func TestBuildAppSettingChangesKeepsNumericLookingSecretQuoted(t *testing.T) {
	changes, err := buildAppSettingChanges([]string{"vk.token@-"}, func() (string, error) {
		return "1234567890", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := changes[0].ValueJson; got != `"1234567890"` {
		t.Fatalf("numeric-looking secret = %s; want a JSON string", got)
	}
	var decoded any
	if err := json.Unmarshal([]byte(changes[0].ValueJson), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded.(string); !ok {
		t.Fatalf("decoded value is %T, want string", decoded)
	}
}

func TestBuildAppSettingChangesReadsOneLineAndStripsCRLF(t *testing.T) {
	withStdin(t, "token-value\r\nignored-second-line\n")
	changes, err := buildAppSettingChanges([]string{"vk.token=-"}, readSettingValue)
	if err != nil {
		t.Fatal(err)
	}
	if got := changes[0].ValueJson; got != `"token-value"` {
		t.Fatalf("value JSON = %s", got)
	}
}

func TestBuildAppSettingChangesRejectsEmptyStdin(t *testing.T) {
	withStdin(t, "")
	if _, err := buildAppSettingChanges([]string{"vk.token@-"}, readSettingValue); err == nil || !strings.Contains(err.Error(), "no value on stdin") {
		t.Fatalf("empty stdin = %v", err)
	}
	withStdin(t, "\n")
	if _, err := buildAppSettingChanges([]string{"vk.token@-"}, readSettingValue); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty line = %v", err)
	}
}

func TestBuildAppSettingChangesRejectsMixedForms(t *testing.T) {
	for _, pairs := range [][]string{
		{"vk.token@-", "other=1"},
		{"other=1", "vk.token@-"},
		{"a@-", "b@-"},
	} {
		_, err := buildAppSettingChanges(pairs, func() (string, error) {
			t.Fatal("stdin was read despite a rejected mixture")
			return "", nil
		})
		if err == nil || !strings.Contains(err.Error(), "only setting") {
			t.Fatalf("%v = %v", pairs, err)
		}
	}
}

func TestBuildAppSettingChangesKeepsKeyValueSniffing(t *testing.T) {
	changes, err := buildAppSettingChanges([]string{"count=42", "name=hello"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].ValueJson != "42" || changes[1].ValueJson != `"hello"` {
		t.Fatalf("changes = %+v", changes)
	}
}
