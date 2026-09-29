package ipmi

import (
	"testing"

	goipmi "github.com/bougou/go-ipmi/pkg/client"
	"github.com/bougou/go-ipmi/pkg/command/chassis"
	"github.com/bougou/go-ipmi/pkg/types"

	"github.com/nexterm/nexterm/internal/model"
)

func TestMapPowerAction(t *testing.T) {
	cases := map[string]chassis.ChassisControl{
		"on":    chassis.ChassisControlPowerUp,
		"off":   chassis.ChassisControlPowerDown,
		"cycle": chassis.ChassisControlPowerCycle,
		"reset": chassis.ChassisControlHardReset,
		"soft":  chassis.ChassisControlSoftShutdown,
	}
	for action, want := range cases {
		got, ok := mapPowerAction(action)
		if !ok || got != want {
			t.Errorf("mapPowerAction(%q) = %v ok=%v, want %v", action, got, ok, want)
		}
	}
	if _, ok := mapPowerAction("explode"); ok {
		t.Error("unknown action should not map")
	}
}

func TestMapPrivilege(t *testing.T) {
	if mapPrivilege("user") != types.PrivilegeLevelUser {
		t.Error("user")
	}
	if mapPrivilege("operator") != types.PrivilegeLevelOperator {
		t.Error("operator")
	}
	if mapPrivilege("") != types.PrivilegeLevelAdministrator {
		t.Error("default should be administrator")
	}
	if mapPrivilege("administrator") != types.PrivilegeLevelAdministrator {
		t.Error("administrator")
	}
}

func TestParamsFrom(t *testing.T) {
	conn := &model.Connection{
		Host:     "10.0.0.9",
		Username: "ADMIN",
		Options: model.Options{
			"ipmiInterface":  "lanplus",
			"cipherSuite":    3,
			"privilegeLevel": "operator",
		},
	}
	p := paramsFrom(conn, map[string]string{model.SecretPassword: "pw"})
	if p.host != "10.0.0.9" || p.username != "ADMIN" || p.password != "pw" {
		t.Fatalf("params = %+v", p)
	}
	if p.port != 623 {
		t.Errorf("default port = %d, want 623", p.port)
	}
	if !p.hasCipher || p.cipherSuite != 3 {
		t.Errorf("cipher = %d has=%v", p.cipherSuite, p.hasCipher)
	}
	if p.privilege != types.PrivilegeLevelOperator {
		t.Errorf("privilege = %v", p.privilege)
	}
}

func TestNewClientBuilds(t *testing.T) {
	p := paramsFrom(&model.Connection{Host: "1.2.3.4", Username: "u", Options: model.Options{"cipherSuite": 17}}, map[string]string{model.SecretPassword: "p"})
	cl, err := newClient(p)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if cl == nil {
		t.Fatal("nil client")
	}
	if cl.Host != "1.2.3.4" || cl.Port != 623 {
		t.Errorf("client host/port = %s:%d", cl.Host, cl.Port)
	}
	if cl.Interface != goipmi.InterfaceLanplus {
		t.Errorf("interface = %v, want lanplus", cl.Interface)
	}
}
