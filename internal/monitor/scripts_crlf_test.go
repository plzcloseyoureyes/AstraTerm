package monitor

import (
	"io/fs"
	"strings"
	"testing"
)

// POSIX scripts are sent to remote /bin/sh: they must never carry a CR, whatever line endings the checkout used.
func TestScriptsReachShellWithoutCR(t *testing.T) {
	names, err := fs.Glob(scriptFS, "scripts/*.sh")
	if err != nil || len(names) == 0 {
		t.Fatalf("no embedded scripts: %v", err)
	}
	for _, n := range names {
		if strings.Contains(script(strings.TrimPrefix(n, "scripts/")), "\r") {
			t.Errorf("%s contains a carriage return", n)
		}
	}
}
