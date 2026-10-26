package mirror

import (
	"context"
	"errors"
	"strings"

	"github.com/git-pkgs/proxy/internal/handler"
)

// ChartMirrorer caches Helm charts from the configured repositories the way
// the Helm handler serves them.
type ChartMirrorer interface {
	MirrorChart(ctx context.Context, repository, chart, version string) (*handler.CacheResult, error)
}

var (
	errHelmNoRepository = errors.New("helm charts need the upstream.helm repository as the namespace, such as pkg:helm/{repository}/{chart}@{version}")
	errHelmUnavailable  = errors.New("mirroring Helm charts isn't available here")
)

// mirrorHelm mirrors a chart release. The PURL namespace names the
// upstream.helm repository, since the handler caches charts per repository.
func (m *Mirror) mirrorHelm(ctx context.Context, pv PackageVersion, tracker *progressTracker) {
	repository, chart, ok := strings.Cut(pv.Name, "/")
	if !ok || repository == "" || chart == "" {
		m.recordFailure(tracker, pv, errHelmNoRepository)
		return
	}
	if m.Charts == nil {
		m.recordFailure(tracker, pv, errHelmUnavailable)
		return
	}
	result, err := m.Charts.MirrorChart(ctx, repository, chart, pv.Version)
	if err != nil {
		m.recordFailure(tracker, pv, err)
		return
	}
	m.recordResult(tracker, pv, result)
}
