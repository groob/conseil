package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

type api struct {
	grants grantStore
	minter *githubMinter
	logger *slog.Logger
}

func (a *api) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /token", a.handleToken)
	return mux
}

func (a *api) handleToken(response http.ResponseWriter, request *http.Request) {
	sourceVM := request.Header.Get("X-Exedev-Source-Vm")
	repository, err := a.grants.lookup(sourceVM)
	if err != nil {
		a.logger.Warn("denying ungranted peer VM", "source_vm", sourceVM, "error", err)
		http.Error(response, "peer VM is not allowed", http.StatusForbidden)
		return
	}

	token, err := a.minter.mint(request.Context(), repository)
	if err != nil {
		a.logger.Error("issuing installation token", "source_vm", sourceVM, "error", err)
		http.Error(response, "could not issue installation token", http.StatusBadGateway)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Pragma", "no-cache")
	writeJSON(response, http.StatusOK, token)
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	contents, err := json.Marshal(value)
	if err != nil {
		http.Error(response, "could not encode response", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_, _ = response.Write(append(contents, '\n'))
}
