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
	file, err := os.OpenFile("mock_data.txt", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open file to save mock data: %w", err)
	}
	defer file.Close()

	// 1GB.
	const fileSize = 1024 * 1024 * 1024
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
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger.Info("new http request")
		w.WriteHeader(http.StatusOK)
	})
	http.Handle("/mock-data/", http.StripPrefix("/mock-data", http.FileServer(http.Dir("/mock_data"))))
	logger.Info("starting http server")
	if err := http.ListenAndServe("0.0.0.0:44000", nil); err != nil {
		return fmt.Errorf("failed to listen and serve: %w", err)
	}
	return nil
}
