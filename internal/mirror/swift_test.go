package mirror

import "testing"

func TestSwiftIdentity(t *testing.T) {
	for name, want := range map[string]string{
		"apple/swift-nio":            "apple.swift-nio",
		"github.com/apple/swift-nio": "",
		"swift-nio":                  "",
	} {
		got, err := swiftIdentity(name)
		if got != want || (want == "") != (err != nil) {
			t.Errorf("swiftIdentity(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
}
