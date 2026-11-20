package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func main() {
	port, _ := strconv.Atoi(getenv("MINATO_RUNTIME_PORT", "4300"))
	server := runtime.NewServer(runtime.Config{
		Token:         getenv("MINATO_RUNTIME_TOKEN", ""),
		HTTPClient:    &http.Client{Timeout: 120 * time.Second},
		MaxBodyBytes:  8 << 20,
		MaxConcurrent: getint("MINATO_RUNTIME_MAX_CONCURRENT", 128),
	})
	log.Printf("minato-runtime listening on :%d", port)
	if err := http.ListenAndServe(":"+strconv.Itoa(port), server); err != nil {
		log.Fatal(err)
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getint(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}
