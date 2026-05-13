package main

import (
	"encoding/json"
	"testing"
)

func TestParsePutProgramEnvBody_flatJSON(t *testing.T) {
	raw := []byte(`{"CLOUDPHONE_UNICOM_MANUAL_API_KEY":"sk-test"}`)
	b, err := parsePutProgramEnvBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.Vars["CLOUDPHONE_UNICOM_MANUAL_API_KEY"] != "sk-test" {
		t.Fatalf("got %#v", b.Vars)
	}
}

func TestParsePutProgramEnvBody_varsAndFlat(t *testing.T) {
	raw := []byte(`{"vars":{"A":"1"},"B":"2","merge":false}`)
	b, err := parsePutProgramEnvBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.Vars["A"] != "1" || b.Vars["B"] != "2" {
		t.Fatalf("got %#v", b.Vars)
	}
	if b.Merge == nil || *b.Merge != false {
		t.Fatalf("merge: %#v", b.Merge)
	}
}

func TestParsePutProgramEnvBody_remove(t *testing.T) {
	raw := []byte(`{"remove":["X"],"vars":{"Y":"z"}}`)
	b, err := parsePutProgramEnvBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Remove) != 1 || b.Remove[0] != "X" {
		t.Fatalf("remove: %#v", b.Remove)
	}
	if b.Vars["Y"] != "z" {
		t.Fatalf("vars: %#v", b.Vars)
	}
}

func TestParsePutProgramEnvBody_legacyOnlyVars(t *testing.T) {
	raw := []byte(`{"vars":{"CLOUDPHONE_UNICOM_MANUAL_API_KEY":"sk-legacy"}}`)
	b, err := parsePutProgramEnvBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.Vars["CLOUDPHONE_UNICOM_MANUAL_API_KEY"] != "sk-legacy" {
		t.Fatalf("got %#v", b.Vars)
	}
}

func TestParsePutProgramEnvBody_invalidKey(t *testing.T) {
	raw := []byte(`{"9bad":"x"}`)
	if _, err := parsePutProgramEnvBody(raw); err == nil {
		t.Fatal("expected error")
	}
}

func TestProgramEnvReplyJSONRoundTrip(t *testing.T) {
	reply := programEnvReply{
		Success: true,
		Vars:    map[string]string{"K": "secret"},
	}
	buf, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(buf, &m)
	if m["vars"].(map[string]any)["K"] != "secret" {
		t.Fatalf("%s", string(buf))
	}
}
