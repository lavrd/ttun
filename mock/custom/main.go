package main

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/alecthomas/kong"
)

type Cmd struct {
	Gen    CmdGen    `cmd:"" help:"Generate mock data."`
	Server CmdServer `cmd:"" help:"Start mock data server."`
}

type CmdGen struct {
	Size int `default:"50" help:"Mock data size in megabytes."`
}

func (cmd *CmdGen) Run() error {
	file, err := os.OpenFile("../data/bytes.txt", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("failed to open file to save mock data: %w", err)
	}
	defer file.Close()

	const bufferSize = 1024 * 1024 // 1MB
	buffer := make([]byte, bufferSize)
	for i := 0; i < cmd.Size; i++ {
		if _, err = rand.Read(buffer); err != nil {
			return fmt.Errorf("failed to fill buffer with random data: %w", err)
		}
		if _, err = file.Write(buffer); err != nil {
			return fmt.Errorf("failed to save buffer to file: %w", err)
		}
	}

	return nil
}

type CmdServer struct{}

func (cmd *CmdServer) Run() error {
	logger := slog.Default()
	loggerMiddleware := &LoggerMiddleware{logger: logger}
	http.Handle("/health", loggerMiddleware.Handler(http.HandlerFunc(healthHandler)))
	http.Handle("/data/", loggerMiddleware.Handler(http.StripPrefix("/data", http.FileServer(http.Dir("/data")))))
	logger.Info("starting http server")
	if err := http.ListenAndServe("0.0.0.0:44000", nil); err != nil {
		return fmt.Errorf("failed to listen and serve: %w", err)
	}
	return nil
}

func main() {
	kctx := kong.Parse(&Cmd{})
	if err := kctx.Run(); err != nil {
		slog.Error("failed to run command", "error", err)
		os.Exit(1)
	}
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

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
