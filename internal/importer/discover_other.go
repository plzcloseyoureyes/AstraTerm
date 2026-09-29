//go:build !windows

package importer

// registrySources lists Windows registry session stores; there are none on other platforms.
func registrySources() []discoverEntry { return nil }

// readRegistrySource renders a registry session store; unavailable on other platforms.
func readRegistrySource(string) ([]byte, string, bool) { return nil, "", false }
