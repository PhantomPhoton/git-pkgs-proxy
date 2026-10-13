package server

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/git-pkgs/proxy/internal/handler"
	"github.com/go-chi/chi/v5"
)

// HostedAPIHandler serves /api/hosted, where operators push archives that the
// proxy serves in place of, or in addition to, what the upstream has.
type HostedAPIHandler struct {
	proxy          *handler.Proxy
	maxUploadBytes int64
	logger         *slog.Logger
}

// NewHostedAPIHandler creates the /api/hosted handler.
func NewHostedAPIHandler(proxy *handler.Proxy, maxUploadBytes int64, logger *slog.Logger) *HostedAPIHandler {
	return &HostedAPIHandler{proxy: proxy, maxUploadBytes: maxUploadBytes, logger: logger}
}

// HostedPushResponse describes a pushed archive.
type HostedPushResponse struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	handler.HostedArtifact
}

// HostedListResponse lists the archives pushed for a package.
type HostedListResponse struct {
	Ecosystem string                   `json:"ecosystem"`
	Name      string                   `json:"name"`
	Artifacts []handler.HostedArtifact `json:"artifacts"`
}

// registerHostedRoutes mounts /api/hosted. Listing is open like the rest of
// the API; pushing and deleting require the bearer token.
func registerHostedRoutes(r chi.Router, h *HostedAPIHandler, token string) {
	r.Get("/api/hosted/{ecosystem}/*", h.HandleList)
	r.Group(func(w chi.Router) {
		w.Use(requireBearerToken(token))
		w.Put("/api/hosted/{ecosystem}/*", h.HandlePush)
		w.Delete("/api/hosted/{ecosystem}/*", h.HandleDelete)
	})
}

// HandlePush handles PUT /api/hosted/{ecosystem}/{name}/{version}
// @Summary Push a package archive
// @Description Stores the request body as the archive for name@version. It is served in place of the upstream archive with the same version and is never evicted.
// @Tags hosted
// @Accept octet-stream
// @Produce json
// @Security BearerAuth
// @Param ecosystem path string true "Ecosystem (npm or composer)"
// @Param name path string true "Package name, including an npm scope or the Composer vendor"
// @Param version path string true "Version"
// @Success 201 {object} HostedPushResponse
// @Failure 400 {object} ErrorResponse
// @Failure 401 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Failure 413 {object} ErrorResponse
// @Router /api/hosted/{ecosystem}/{name}/{version} [put]
func (h *HostedAPIHandler) HandlePush(w http.ResponseWriter, r *http.Request) {
	ecosystem, name, version, ok := hostedVersionPath(w, r)
	if !ok {
		return
	}
	target, err := handler.HostedTargetFor(ecosystem, name, version)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	body := bufio.NewReader(http.MaxBytesReader(w, r.Body, h.maxUploadBytes))
	if _, err := body.Peek(1); errors.Is(err, io.EOF) {
		badRequest(w, "request body is empty")
		return
	}

	pushed, err := h.proxy.PushLocalArtifact(r.Context(), ecosystem, target, body, r.Header.Get("Content-Type"))
	if err != nil {
		h.pushError(w, err)
		return
	}
	h.logger.Info("pushed package archive",
		"ecosystem", ecosystem, "name", target.Name, "version", target.Version,
		"filename", target.Filename, "sha256", pushed.SHA256)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, HostedPushResponse{Ecosystem: ecosystem, Name: target.Name, HostedArtifact: pushed})
}

func (h *HostedAPIHandler) pushError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, ErrCodeBadRequest, "archive exceeds hosted.max_upload_size")
	case errors.Is(err, handler.ErrVersionDenied), errors.Is(err, handler.ErrArtifactBlocked):
		writeError(w, http.StatusForbidden, ErrCodeBadRequest, err.Error())
	default:
		h.logger.Error("failed to push package archive", "error", err)
		internalError(w, "failed to store archive")
	}
}

// HandleList handles GET /api/hosted/{ecosystem}/{name}
// @Summary List pushed package archives
// @Tags hosted
// @Produce json
// @Param ecosystem path string true "Ecosystem"
// @Param name path string true "Package name"
// @Success 200 {object} HostedListResponse
// @Failure 400 {object} ErrorResponse
// @Router /api/hosted/{ecosystem}/{name} [get]
func (h *HostedAPIHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	ecosystem := chi.URLParam(r, "ecosystem")
	segments, err := packagePathSegments(r)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	name := strings.Join(segments, "/")
	if !handler.HostedSupported(ecosystem) {
		badRequest(w, handler.ErrHostedUnsupported.Error())
		return
	}

	listed, err := h.proxy.LocalArtifacts(ecosystem, name)
	if err != nil {
		h.logger.Error("failed to list pushed archives", "error", err)
		internalError(w, "failed to list pushed archives")
		return
	}
	writeJSON(w, HostedListResponse{Ecosystem: ecosystem, Name: name, Artifacts: listed})
}

// HandleDelete handles DELETE /api/hosted/{ecosystem}/{name}/{version}
// @Summary Delete a pushed version
// @Description Removes the archives pushed for name@version. If the upstream has the version, clients get the upstream archive again.
// @Tags hosted
// @Security BearerAuth
// @Param ecosystem path string true "Ecosystem"
// @Param name path string true "Package name"
// @Param version path string true "Version"
// @Success 204
// @Failure 401 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/hosted/{ecosystem}/{name}/{version} [delete]
func (h *HostedAPIHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	ecosystem, name, version, ok := hostedVersionPath(w, r)
	if !ok {
		return
	}
	err := h.proxy.DeleteLocalVersion(ecosystem, name, version)
	switch {
	case errors.Is(err, handler.ErrHostedVersionNotFound):
		notFound(w, err.Error())
	case err != nil:
		h.logger.Error("failed to delete pushed version", "error", err)
		internalError(w, "failed to delete pushed version")
	default:
		h.logger.Info("deleted pushed version", "ecosystem", ecosystem, "name", name, "version", version)
		w.WriteHeader(http.StatusNoContent)
	}
}

// minVersionPathSegments is one name segment plus the version.
const minVersionPathSegments = 2

// hostedVersionPath splits {name}/{version} from the wildcard. The version is
// always the last segment; the name may span several (npm scopes, Composer
// vendors).
func hostedVersionPath(w http.ResponseWriter, r *http.Request) (ecosystem, name, version string, ok bool) {
	segments, err := packagePathSegments(r)
	if err != nil {
		badRequest(w, err.Error())
		return "", "", "", false
	}
	if len(segments) < minVersionPathSegments {
		badRequest(w, "path must be /api/hosted/{ecosystem}/{name}/{version}")
		return "", "", "", false
	}
	last := len(segments) - 1
	return chi.URLParam(r, "ecosystem"), strings.Join(segments[:last], "/"), segments[last], true
}
