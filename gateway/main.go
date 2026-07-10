package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (rr *responseRecorder) WriteHeader(code int) {
	rr.status = code
	rr.ResponseWriter.WriteHeader(code)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rr := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rr, r)
		log.Printf("[http] %s %s status=%d duration=%v", r.Method, r.URL.Path, rr.status, time.Since(start))
	})
}

var logBuffer = newLogRingBuffer(10000)

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	flag.Parse()

	logFile, err := os.OpenFile(filepath.Join(filepath.Dir(*configPath), "gateway.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("failed to open log file: %v", err)
	}
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stderr, logFile, logBuffer))

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	llm := NewLLMClient(cfg)
	log.Printf("LLM API key env %s present: %v", cfg.LLM.APIKeyEnv, cfg.APIKey() != "")
	store := NewStreamStore()
	store.StartCleanup(5*time.Minute, time.Duration(cfg.Server.StreamStoreTTLSeconds)*time.Second)
	mcpManager := NewMCPManager(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := mcpManager.Initialize(ctx); err != nil {
		log.Printf("warning: failed to initialize MCP servers: %v", err)
	}
	cancel()

	handlers := NewHandlers(cfg, llm, store, mcpManager)

	mux := http.NewServeMux()
	handlers.RegisterRoutes(mux)

	loggedHandler := loggingMiddleware(mux)

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	server := &http.Server{
		Addr:    addr,
		Handler: loggedHandler,
	}

	go func() {
		log.Printf("starting AI assistant gateway on %s", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down gateway")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
	store.Stop()
	if err := mcpManager.Close(); err != nil {
		log.Printf("mcp shutdown error: %v", err)
	}
}
