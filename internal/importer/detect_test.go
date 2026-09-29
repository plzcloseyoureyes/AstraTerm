package importer

import "testing"

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		fixture string
		want    string
	}{
		{"mobaxterm.mxtsessions", fmtMobaXterm},
		{"putty.reg", fmtPuttyReg},
		{"ssh_config", fmtSSHConfig},
		{"termius.csv", fmtTermiusCSV},
		{"mremoteng.xml", fmtMRemoteNG},
		{"filezilla.xml", fmtFileZilla},
		{"winscp.ini", fmtWinSCP},
		{"securecrt.xml", fmtSecureCRT},
		{"known_hosts", fmtKnownHosts},
		{"nexterm.json", fmtJSON},
		{"remmina.remmina", fmtRemmina},
	}
	for _, tc := range cases {
		got := detectFormat(loadFixtureBytes(t, tc.fixture))
		if got != tc.want {
			t.Errorf("detectFormat(%s) = %q, want %q", tc.fixture, got, tc.want)
		}
	}
}

func TestDetectAutoParses(t *testing.T) {
	// "auto" must route each fixture to a working parser.
	for _, f := range []string{"mobaxterm.mxtsessions", "putty.reg", "ssh_config", "termius.csv", "mremoteng.xml",
		"filezilla.xml", "winscp.ini", "securecrt.xml", "nexterm.json"} {
		if _, err := parseSource(fmtAuto, loadFixtureBytes(t, f), previewOptions{}); err != nil {
			t.Errorf("auto parse %s: %v", f, err)
		}
	}
}

func loadFixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	return loadFixture(t, name)
}
