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
	statePath := flag.String("model-state", os.Getenv("MINATO_RUNTIME_MODEL_STATE"), "Optional path for persisted dynamic models")
	keyStatePath := flag.String("key-state", os.Getenv("MINATO_RUNTIME_KEY_STATE"), "Optional path for persisted virtual keys")
	port := flag.Int("port", getint("MINATO_RUNTIME_PORT", 4300), "HTTP listener port")
	host := flag.String("host", "0.0.0.0", "HTTP listener host")
	flag.Parse()
	config := runtime.Config{
		Token:         getenv("MINATO_RUNTIME_TOKEN", ""),
		HTTPClient:    &http.Client{Timeout: 120 * time.Second},
		MaxBodyBytes:  8 << 20,
		MaxConcurrent: getint("MINATO_RUNTIME_MAX_CONCURRENT", 128),
	}
	var proxy proxyconfig.Config
	if *configPath != "" {
		file, err := os.Open(*configPath)
		if err != nil {
			log.Fatal(err)
		}
		proxy, err = proxyconfig.Load(file)
		file.Close()
		if err != nil {
			log.Fatal(err)
		}
		config.Routes = proxy.Routes
		config.NumRetries = proxy.NumRetries
		config.RoutingStrategy = proxy.RoutingStrategy
		if proxy.MasterKey != "" {
			config.Token = proxy.MasterKey
		}
		config.AuthorizeInference = true
		if proxy.Timeout > 0 {
			config.HTTPClient.Timeout = proxy.Timeout
		}
	}
	var keys *proxyconfig.KeyRegistry
	if *configPath != "" && config.Token != "" {
		var err error
		keys, err = proxyconfig.LoadKeys(config.Token, *keyStatePath)
		if err != nil {
			log.Fatal(err)
		}
		config.AuthorizeKey = keys.Authorize
	}
	if *keyStatePath != "" && keys == nil {
		log.Fatal("virtual key state requires standalone config and a master key")
	}
	server := runtime.NewServer(config)
	var handler http.Handler = server
	if *configPath != "" {
		var err error
		handler, err = proxyconfig.WithModelManagement(server, proxy, config.Token, *statePath)
		if err != nil {
			log.Fatal(err)
		}
	}
	if keys != nil {
		handler = keys.Handler(handler)
	}
	address := net.JoinHostPort(*host, strconv.Itoa(*port))
	log.Printf("minato-runtime listening on %s", address)
	if err := http.ListenAndServe(address, handler); err != nil {
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
