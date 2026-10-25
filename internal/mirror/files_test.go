package mirror

import (
	"reflect"
	"testing"
)

func TestPURLSourceCondaChannelAndQualifiers(t *testing.T) {
	versions := &fakeVersions{versions: []string{"1.0", "1.1"}}
	source := &PURLSource{
		PURLs: []string{
			"pkg:conda/conda-forge/numpy@2.1.0?subdir=linux-64&build=py312h_0&type=conda",
			"pkg:conda/numpy@2.1.0?channel=conda-forge&repository_url=https://example.com",
			"pkg:conda/numpy@2.1.0",
			"pkg:conda/numpy",
			"pkg:conda/numpy?channel=bioconda&subdir=noarch",
		},
		Versions: versions,
	}

	got := enumerate(t, source)

	want := []PackageVersion{
		{Ecosystem: "conda", Name: "conda-forge/numpy", Version: "2.1.0",
			Qualifiers: map[string]string{"subdir": "linux-64", "build": "py312h_0", "type": "conda"}},
		{Ecosystem: "conda", Name: "conda-forge/numpy", Version: "2.1.0"},
		{Ecosystem: "conda", Name: "numpy", Version: "2.1.0"},
		{Ecosystem: "conda", Name: "numpy"},
		{Ecosystem: "conda", Name: "bioconda/numpy", Version: "1.0", Qualifiers: map[string]string{"subdir": "noarch"}},
		{Ecosystem: "conda", Name: "bioconda/numpy", Version: "1.1", Qualifiers: map[string]string{"subdir": "noarch"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("items = %+v\nwant %+v", got, want)
	}
	if !reflect.DeepEqual(versions.asked, []string{"conda/bioconda/numpy"}) {
		t.Errorf("listed versions of %v, want only the package with a channel", versions.asked)
	}
}
