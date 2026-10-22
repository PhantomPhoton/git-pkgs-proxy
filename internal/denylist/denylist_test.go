package denylist

import "testing"

func TestPolicy(t *testing.T) {
	p, err := New([]string{"pkg:pypi/My_Package@1.0", "pkg:npm/@scope/name@2.0.0", "pkg:npm/%40scope/name@2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"pkg:pypi/my-package@1.0", "pkg:npm/%40scope/name@2.0.0"} {
		if !p.Denied(value) {
			t.Errorf("not denied: %s", value)
		}
	}
	for _, value := range []string{"pkg:pypi/my-package@1.1", "pkg:pypi/my-package", "pkg:npm/other@2.0.0", "invalid"} {
		if p.Denied(value) {
			t.Errorf("unexpected denial: %s", value)
		}
	}
	versions := p.Versions("pkg:pypi/my-package")
	delete(versions, "1.0")
	if !p.Denied("pkg:pypi/my-package@1.0") {
		t.Fatal("caller mutated policy")
	}
	var empty *Policy
	if empty.Denied("pkg:npm/example@1") || empty.Versions("pkg:npm/example") != nil {
		t.Fatal("nil policy must allow everything")
	}
}

func TestInvalidPolicy(t *testing.T) {
	for _, value := range []string{"", "not-a-purl", "pkg:npm/foo", "pkg:npm/foo@*", "pkg:npm/foo@>=1", "pkg:npm/foo@1?arch=x86", "pkg:npm/foo@1#src", "pkg:conda/numpy@1?subdir=linux-64", "pkg:pypi/foo@1?channel=x"} {
		t.Run(value, func(t *testing.T) {
			if _, err := New([]string{value}); err == nil {
				t.Fatal("invalid denylist accepted")
			}
		})
	}
}

func TestEcosystemSpellings(t *testing.T) {
	p, err := New([]string{
		"pkg:pypi/zope.interface@6.0",
		"pkg:nuget/Newtonsoft.Json@13.0.1-Beta",
		"pkg:gem/nokogiri@1.16.0",
		"pkg:gem/sorbet-static@0.5.1-universal-darwin-22",
		"pkg:conda/numpy@1.26.4",
		"pkg:conda/scipy@1.0?channel=conda-forge",
		"pkg:conda/bioconda/samtools@1.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"pkg:pypi/zope-interface@6.0", "pkg:pypi/Zope__Interface@6.0",
		"pkg:nuget/newtonsoft.json@13.0.1-beta",
		"pkg:gem/nokogiri@1.16.0", "pkg:gem/nokogiri@1.16.0-x86_64-linux",
		"pkg:gem/sorbet-static@0.5.1-universal-darwin-22",
		"pkg:conda/numpy@1.26.4", "pkg:conda/conda-forge%2Fnumpy@1.26.4",
		"pkg:conda/conda-forge%2Fscipy@1.0", "pkg:conda/bioconda%2Fsamtools@1.0",
	} {
		if !p.Denied(value) {
			t.Errorf("not denied: %s", value)
		}
	}
	for _, value := range []string{
		"pkg:pypi/zopeinterface@6.0", "pkg:gem/nokogiri@1.16.1-x86_64-linux", "pkg:gem/Nokogiri@1.16.0",
		"pkg:conda/bioconda%2Fscipy@1.0", "pkg:conda/scipy@1.0", "pkg:conda/conda-forge%2Fsamtools@1.0",
	} {
		if p.Denied(value) {
			t.Errorf("unexpected denial: %s", value)
		}
	}
	if !p.Versions("pkg:pypi/zope_interface")["6.0"] {
		t.Error("pypi versions not found under another spelling")
	}
}
