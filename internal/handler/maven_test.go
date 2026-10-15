package handler

import (
	"log/slog"
	"testing"
)

func TestMavenParsePath(t *testing.T) {
	h := &MavenHandler{proxy: &Proxy{Logger: slog.Default()}}

	tests := []struct {
		path         string
		wantGroup    string
		wantArtifact string
		wantVersion  string
		wantFilename string
	}{
		{
			"com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar",
			"com.google.guava", "guava", "32.1.3-jre", "guava-32.1.3-jre.jar",
		},
		{
			"org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.jar",
			"org.apache.commons", "commons-lang3", "3.14.0", "commons-lang3-3.14.0.jar",
		},
		{
			"junit/junit/4.13.2/junit-4.13.2.jar",
			"junit", "junit", "4.13.2", "junit-4.13.2.jar",
		},
		{
			"short/path",
			"", "", "", "",
		},
	}

	for _, tt := range tests {
		group, artifact, version, filename := h.parsePath(tt.path)
		if group != tt.wantGroup || artifact != tt.wantArtifact || version != tt.wantVersion || filename != tt.wantFilename {
			t.Errorf("parsePath(%q) = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
				tt.path, group, artifact, version, filename,
				tt.wantGroup, tt.wantArtifact, tt.wantVersion, tt.wantFilename)
		}
	}
}

func TestMavenIsArtifactFile(t *testing.T) {
	h := &MavenHandler{}

	tests := []struct {
		filename string
		want     bool
	}{
		{"guava-32.1.3-jre.jar", true},
		{"guava-32.1.3-jre.pom", true},
		{"guava-32.1.3-jre.module", true},
		{"app-1.0.war", true},
		{"lib-1.0.aar", true},
		{"apache-maven-3.9.9-bin.tar.gz", true},
		{"apache-maven-3.9.9-bin.zip", true},
		{"maven-mvnd-1.0.6-linux-amd64.tgz", true},
		{"maven-metadata.xml", false},
		{"guava-32.1.3-jre.jar.sha1", false},
		{"apache-maven-3.9.9-bin.tar.gz.sha512", false},
	}

	for _, tt := range tests {
		got := h.isArtifactFile(tt.filename)
		if got != tt.want {
			t.Errorf("isArtifactFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestMavenIsVersionListingFile(t *testing.T) {
	h := &MavenHandler{}

	tests := []struct {
		filename string
		want     bool
	}{
		{"maven-metadata.xml", true},
		{"maven-metadata.xml.sha1", true},
		{"maven-metadata.xml.sha256", true},
		{"guava-32.1.3-jre.jar", false},
		{"guava-32.1.3-jre.jar.sha1", false},
		{"com.diffplug.spotless.gradle.plugin-8.4.0.pom", false},
	}

	for _, tt := range tests {
		if got := h.isVersionListingFile(tt.filename); got != tt.want {
			t.Errorf("isVersionListingFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}

func TestMavenIsChecksumFile(t *testing.T) {
	h := &MavenHandler{}

	tests := []struct {
		filename string
		want     bool
	}{
		{"guava-32.1.3-jre.jar.sha1", true},
		{"guava-32.1.3-jre.jar.md5", true},
		{"guava-32.1.3-jre.pom.sha1", true},
		{"guava-32.1.3-jre.jar.asc", true},
		{"apache-maven-3.9.9-bin.tar.gz.sha512", true},
		{"com.diffplug.spotless.gradle.plugin-8.4.0.pom.sha1", true},
		{"guava-32.1.3-jre.jar", false},
		{"guava-32.1.3-jre.pom", false},
		{"maven-metadata.xml", false},
	}

	for _, tt := range tests {
		if got := h.isChecksumFile(tt.filename); got != tt.want {
			t.Errorf("isChecksumFile(%q) = %v, want %v", tt.filename, got, tt.want)
		}
	}
}
