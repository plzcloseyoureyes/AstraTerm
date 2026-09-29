package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestNewID(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := NewID()
		if len(id) != 20 || !ValidID(id) || seen[id] {
			t.Fatalf("bad id %q", id)
		}
		seen[id] = true
	}
	if ValidID("") || ValidID("../etc") || ValidID("UPPER") {
		t.Fatal("ValidID accepted junk")
	}
}

func TestOptions(t *testing.T) {
	var o Options
	if err := json.Unmarshal([]byte(`{"term":"","baud":"115200","monitoring":false,"jumpHosts":["a","b"],
		"env":{"A":"1","B":2},"proxy":{"type":"socks5","port":1080},"agentForwarding":"yes","keepAliveSec":30}`), &o); err != nil {
		t.Fatal(err)
	}
	if o.String("term") != "xterm-256color" || o.String("encoding") != "utf-8" || o.String("x", "d") != "d" {
		t.Fatal("string defaults")
	}
	if o.Int("baud") != 115200 || o.Int("dataBits") != 8 || o.Int("keepAliveSec") != 30 || o.Int("missing", 7) != 7 {
		t.Fatal("ints")
	}
	if o.Bool("monitoring") || !o.Bool("negotiate") || !o.Bool("agentForwarding") || o.Bool("x") {
		t.Fatal("bools")
	}
	if s := o.Strings("jumpHosts"); len(s) != 2 || s[1] != "b" {
		t.Fatalf("strings: %v", s)
	}
	if env := o.StringMap("env"); env["A"] != "1" || env["B"] != "2" {
		t.Fatalf("string map: %v", env)
	}
	var proxy struct {
		Type string `json:"type"`
		Port int    `json:"port"`
	}
	if err := o.Decode("proxy", &proxy); err != nil || proxy.Port != 1080 {
		t.Fatalf("decode: %v %+v", err, proxy)
	}
	cl := o.Clone()
	cl.Map("proxy")["port"] = 1
	if o.Map("proxy")["port"] != float64(1080) {
		t.Fatal("clone is shallow")
	}
	var nilOpts Options
	b, _ := json.Marshal(nilOpts)
	if string(b) != "{}" {
		t.Fatalf("nil options marshal: %s", b)
	}
}

func TestErrorsAndConnection(t *testing.T) {
	wrapped := fmt.Errorf("ctx: %w", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) || errors.Is(wrapped, ErrConflict) {
		t.Fatal("errors.Is on codes")
	}
	c := &Connection{Protocol: ProtoRDP, Host: "::1"}
	if c.Address() != "[::1]:3389" {
		t.Fatalf("address: %s", c.Address())
	}
	c.Normalize()
	b, _ := json.Marshal(c)
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["tags"] == nil || m["secretKeys"] == nil || m["options"] == nil || m["authMethod"] != "auto" {
		t.Fatalf("normalized JSON: %s", b)
	}
	if _, ok := m["secrets"]; ok {
		t.Fatal("empty secrets serialized")
	}
	if KindForProtocol(ProtoVNC) != KindVNC || KindForProtocol(ProtoSSH) != KindTerminal {
		t.Fatal("kinds")
	}
	if !ValidProtocol("ssh") || !ValidProtocol("x-custom") || ValidProtocol("SSH") || ValidProtocol("-x") {
		t.Fatal("ValidProtocol")
	}
}
