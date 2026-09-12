package main

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Router claims Command Code models for this plugin's executor.
//
// Model registration alone already routes `<provider>/<id>` requests here (the
// host registers the executor under this plugin's provider key), so the router
// exists for the cases registration cannot cover: a bare upstream model id or a
// config-declared alias appearing in the request before provider resolution.
type Router struct {
	cfg *pluginConfig
}

func NewRouter(cfg *pluginConfig) *Router { return &Router{cfg: cfg} }

// owned reports whether this plugin should serve the request.
func (r *Router) owned(req pluginapi.ModelRouteRequest) bool {
	for _, candidate := range []string{req.RequestedModel, modelFromBody(req.Body)} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(candidate, Provider+"/"); ok && rest != "" {
			return true
		}
		if r.cfg.claimsModel(candidate) {
			return true
		}
	}
	return false
}

// RouteModel routes owned models to this plugin's own executor.
func (r *Router) RouteModel(_ context.Context, req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, error) {
	if !r.owned(req) {
		return pluginapi.ModelRouteResponse{}, nil
	}
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "commandcode-go: Command Code Go plan via /alpha/generate",
	}, nil
}

// modelFromBody extracts the model field from a raw client payload, so routing
// still matches when RequestedModel was already rewritten by the host.
func modelFromBody(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if errUnmarshal := jsonUnmarshal(body, &probe); errUnmarshal != nil {
		return ""
	}
	return probe.Model
}
