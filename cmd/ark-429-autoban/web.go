package main

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"

	"github.com/wyx1818/ark-429-autoban/internal/cpasdk/pluginapi"
)

//go:embed web/status.html web/status.css web/status.js
var webAssets embed.FS

var statusTemplate = template.Must(template.ParseFS(webAssets, "web/status.html"))

type statusPageData struct {
	Plugin  string
	Version string
	Status  managementBanStatus
}

func (p *plugin) statusPageResponse() pluginapi.ManagementResponse {
	var body bytes.Buffer
	err := statusTemplate.Execute(&body, statusPageData{Plugin: pluginName, Version: pluginVersion})
	if err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]any{
			"error": "template_error", "message": err.Error(),
		})
	}
	return contentResponse(http.StatusOK, "text/html; charset=utf-8", body.Bytes())
}

func embeddedAssetResponse(path, contentType string) pluginapi.ManagementResponse {
	raw, err := webAssets.ReadFile(path)
	if err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]any{
			"error": "asset_error", "message": err.Error(),
		})
	}
	return contentResponse(http.StatusOK, contentType, raw)
}

func contentResponse(status int, contentType string, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{contentType}},
		Body:       body,
	}
}
