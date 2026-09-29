package server_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/nexterm/nexterm/internal/server/servertest"
)

// Concurrent merge patches of the same settings section (several tabs saving at once) must all survive: the
// read-merge-write is one transaction. Regression: the handler read, merged and wrote in separate steps, so
// concurrent PUTs overwrote each other's keys.
func TestSettingsConcurrentPatchesDoNotLoseUpdates(t *testing.T) {
	env := servertest.New(t)
	admin := env.Setup("admin", pass)

	const n = 24
	for _, scope := range []string{"/api/settings", "/api/admin/settings"} {
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				body := map[string]any{"race": map[string]any{fmt.Sprintf("k%02d", i): i}}
				if code := admin.JSON("PUT", scope, body, nil); code != 200 {
					errs <- fmt.Errorf("PUT %s #%d: status %d", scope, i, code)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		var got map[string]any
		admin.MustJSON("GET", scope, nil, &got)
		race, _ := got["race"].(map[string]any)
		if len(race) != n {
			t.Fatalf("%s: %d of %d concurrently patched keys survived: %v", scope, len(race), n, race)
		}
		admin.MustJSON("PUT", scope, map[string]any{"race": nil}, nil)
	}
}
