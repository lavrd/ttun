package main

import (
	"log/slog"
	"net/http"
)

func main() {
	logger := slog.Default()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger.Info("new http request")
		w.WriteHeader(http.StatusOK)
	})
	logger.Info("starting http server")
	if err := http.ListenAndServe("0.0.0.0:44000", nil); err != nil {
		logger.Error("failed to listen and serve", "error", err)
		return
	}
}
