package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/rquanx/minato-runtime/internal/proxyconfig"
	"github.com/rquanx/minato-runtime/internal/runtime"
)

func main() {
	configPath := flag.String("config", os.Getenv("MINATO_RUNTIME_PROXY_CONFIG"), "LiteLLM-compatible YAML configuration")
	port := flag.Int("port", getint("MINATO_RUNTIME_PORT", 4300), "HTTP listener port")
	host := flag.String("host", "0.0.0.0", "HTTP listener host")
	flag.Parse()
	config := runtime.Config{
		Token:         getenv("MINATO_RUNTIME_TOKEN", ""),
		HTTPClient:    &http.Client{Timeout: 120 * time.Second},
		MaxBodyBytes:  8 << 20,
		MaxConcurrent: getint("MINATO_RUNTIME_MAX_CONCURRENT", 128),
	}
	if *configPath != "" {
		file, err := os.Open(*configPath)
		if err != nil {
			log.Fatal(err)
		}
		proxy, err := proxyconfig.Load(file)
		file.Close()
		if err != nil {
			log.Fatal(err)
		}
		config.Routes = proxy.Routes
		if proxy.MasterKey != "" {
			config.Token = proxy.MasterKey
		}
		config.AuthorizeInference = true
		if proxy.Timeout > 0 {
			config.HTTPClient.Timeout = proxy.Timeout
		}
	}
	server := runtime.NewServer(config)
	address := net.JoinHostPort(*host, strconv.Itoa(*port))
	log.Printf("minato-runtime listening on %s", address)
	if err := http.ListenAndServe(address, server); err != nil {
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
