package main

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	logger := slog.Default()
	if len(os.Args) != 2 {
		logger.Error("invalid number of arguments to proceed",
			"number", len(os.Args), "required", 2)
		return
	}
	switch os.Args[1] {
	case "gen":
		if err := genMockData(); err != nil {
			logger.Error("failed to generate mock data", "error", err)
			return
		}
	case "run":
		if err := runServer(); err != nil {
			logger.Error("failed to run mock server", "error", err)
			return
		}
	default:
		logger.Error("unknown command for mock-server", "command", os.Args[1])
	}
}

func genMockData() error {
	file, err := os.OpenFile("./data/bytes.txt", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open file to save mock data: %w", err)
	}
	defer file.Close()

	// 50MB.
	const fileSize = 1024 * 1024 * 50
	// 1MB.
	const bufferSize = 1024 * 1024
	buffer := make([]byte, bufferSize)
	for i := int64(0); i < fileSize; i += bufferSize {
		if _, err = rand.Read(buffer); err != nil {
			return fmt.Errorf("failed to fill buffer with random data: %w", err)
		}
		if _, err = file.Write(buffer); err != nil {
			return fmt.Errorf("failed to save buffer to file: %w", err)
		}
	}

	return nil
}

func runServer() error {
	logger := slog.Default()
	loggerMiddleware := &LoggerMiddleware{logger: logger}
	http.Handle("/health", loggerMiddleware.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })))
	http.Handle("/data/", loggerMiddleware.Handler(http.StripPrefix("/data", http.FileServer(http.Dir("/data")))))
	logger.Info("starting http server")
	if err := http.ListenAndServe("0.0.0.0:44000", nil); err != nil {
		return fmt.Errorf("failed to listen and serve: %w", err)
	}
	return nil
}

type LoggerMiddleware struct {
	logger *slog.Logger
}

func (l *LoggerMiddleware) Handler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.logger.Info("http request", "method", r.Method, "uri", r.RequestURI)
		h.ServeHTTP(w, r)
	})
}
