package main

import (
	"database/sql"
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
	importModels := flag.Bool("import-litellm-models", false, "Copy LiteLLM models into empty Go proxy PostgreSQL state and exit")
	importKeys := flag.Bool("import-litellm-keys", false, "Copy supported LiteLLM key digests into empty Go proxy state and exit")
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
		config.LatencyBuffer = proxy.LatencyBuffer
		config.LatencySamples = proxy.LatencySamples
		if proxy.MasterKey != "" {
			config.Token = proxy.MasterKey
		}
		proxy.MasterKey = config.Token
		config.AuthorizeInference = true
		if proxy.Timeout > 0 {
			config.HTTPClient.Timeout = proxy.Timeout
		}
	}
	var stateDB *sql.DB
	if proxy.DatabaseURL != "" {
		if *statePath != "" || *keyStatePath != "" || config.Token == "" {
			log.Fatal("PostgreSQL state requires a master key and separate storage from local state flags")
		}
		var err error
		stateDB, err = proxyconfig.OpenPostgresState(proxy.DatabaseURL)
		if err != nil {
			log.Fatal(err)
		}
		defer stateDB.Close()
	}
	if *importModels || *importKeys {
		if stateDB == nil {
			log.Fatal("LiteLLM import requires general_settings.database_url")
		}
		if *importModels && *importKeys {
			log.Fatal("Run the model and virtual key imports separately")
		}
		if *importKeys {
			count, err := proxyconfig.ImportLiteLLMKeys(stateDB)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Imported %d LiteLLM virtual keys", count)
			return
		}
		count, err := proxyconfig.ImportLiteLLMModels(stateDB, proxy)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Imported %d LiteLLM deployments", count)
		return
	}
	var keys *proxyconfig.KeyRegistry
	if *configPath != "" && config.Token != "" {
		var err error
		if stateDB != nil {
			keys, err = proxyconfig.LoadPostgresKeys(config.Token, stateDB)
		} else {
			keys, err = proxyconfig.LoadKeys(config.Token, *keyStatePath)
		}
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
		if stateDB != nil {
			handler, err = proxyconfig.WithPostgresModelManagement(server, proxy, config.Token, stateDB)
		} else {
			handler, err = proxyconfig.WithModelManagement(server, proxy, config.Token, *statePath)
		}
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
