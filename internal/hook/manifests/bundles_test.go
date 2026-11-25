package manifests

import (
	"slices"
	"testing"
	"testing/fstest"
)

// A bundle built in under a build tag adds its manifest beside the shipped
// ones: every lookup sees it, and a shipped manifest of the same name wins.
func TestABundleAddsItsManifest(t *testing.T) {
	shipped := Names()
	raw, err := Read("claude")
	if err != nil {
		t.Fatal(err)
	}
	saved := bundles
	bundles = append(slices.Clone(bundles), fstest.MapFS{
		"zbundle.json": {Data: []byte(`{"schema":1,"adapter":"zbundle","display_name":"Z Fixture","setup_order":99,"revision":"zbundle-1","install":{"events":["SessionStart"]},"events":{"SessionStart":{}}}`)},
		"claude.json":  {Data: []byte(`{"schema":1,"adapter":"claude","display_name":"Not Claude"}`)},
		"notes.txt":    {Data: []byte("not a manifest")},
	})
	restore := SwapReader(readSource)
	t.Cleanup(func() { bundles = saved; restore() })

	want := append(slices.Clone(shipped), "zbundle")
	slices.Sort(want)
	if got := Names(); !slices.Equal(got, want) {
		t.Fatalf("names = %v", got)
	}
	if !Has("zbundle") || DisplayName("zbundle") != "Z Fixture" || SetupOrder()[len(SetupOrder())-1] != "zbundle" {
		t.Fatalf("bundle manifest not decoded: %q %v", DisplayName("zbundle"), SetupOrder())
	}
	if caps, ok := CapabilitiesAt("zbundle", "zbundle-1"); !ok {
		t.Fatalf("bundle revision not found: %+v", caps)
	}
	if got, err := Read("claude"); err != nil || string(got) != string(raw) || DisplayName("claude") != "Claude Code" {
		t.Fatalf("a bundle replaced a shipped manifest: %q (%v)", DisplayName("claude"), err)
	}
	if _, err := Read("absent"); err == nil {
		t.Fatal("read an adapter no source has")
	}
}
