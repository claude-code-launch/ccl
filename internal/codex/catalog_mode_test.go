package codex

import "testing"

func TestCatalogSourceNeverImpliesDesktopLiveReload(t *testing.T) {
	for _, value := range []string{"", "local", "remote"} {
		mode, err := ParseCatalogMode(value)
		if err != nil {
			t.Fatal(err)
		}
		caps := mode.Capabilities()
		if caps.SupportsLiveReload {
			t.Fatalf("source %q promised unqualified Desktop reload", mode)
		}
		if caps.SupportsRemoteFetch != (mode.Effective() == CatalogRemote) {
			t.Fatalf("source and remote-fetch capability disagree for %q", mode)
		}
	}
	if _, err := ParseCatalogMode("realtime"); err == nil {
		t.Fatal("live-reload terminology was accepted as a catalog source")
	}
}
