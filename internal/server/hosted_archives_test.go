package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
)

// npmTarball builds an npm package tarball holding manifest as
// package/package.json. Building an archive in memory cannot fail, so errors
// panic.
func npmTarball(manifest string) string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	must(tw.WriteHeader(&tar.Header{Name: "package/package.json", Mode: 0o644, Size: int64(len(manifest))}))
	_, err := io.WriteString(tw, manifest)
	must(err)
	must(tw.Close())
	must(gz.Close())
	return buf.String()
}

// composerZip builds a Composer dist archive holding manifest as composer.json.
func composerZip(manifest string) string {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("composer.json")
	must(err)
	_, err = io.WriteString(w, manifest)
	must(err)
	must(zw.Close())
	return buf.String()
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

var (
	requestTarball = npmTarball(`{"name":"request","version":"2.88.3","dependencies":{"qs":"~6.5.2"}}`)
	nodeTarball    = npmTarball(`{"name":"@types/node","version":"20.1.0"}`)
	monologZip     = composerZip(`{"name":"monolog/monolog","require":{"php":">=8.1","psr/log":"^3.0"}}`)
)
