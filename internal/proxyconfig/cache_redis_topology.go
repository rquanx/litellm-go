package proxyconfig

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

type cacheRedisNode struct {
	Host string `json:"host"`
	Port json.RawMessage `json:"port"`
}

func cacheNodeAddress(host string, port json.RawMessage) (string, error) {
	value := strings.Trim(string(port), "\"")
	number, err := strconv.Atoi(value)
	if strings.TrimSpace(host) == "" || err != nil || number < 1 || number > 65535 {
		return "", errors.New("Invalid Redis node address")
	}
	return net.JoinHostPort(host, value), nil
}

func (input cacheConnectionSettings) redisTopology() string {
	if input.RedisType != "" {
		return input.RedisType
	}
	if len(input.StartupNodes) > 0 {
		return "cluster"
	}
	if len(input.SentinelNodes) > 0 {
		return "sentinel"
	}
	return "node"
}

func (input cacheConnectionSettings) newRedisClient() (redis.UniversalClient, error) {
	topology := input.redisTopology()
	if topology == "node" {
		options, err := input.options()
		if err != nil {
			return nil, err
		}
		options.ContextTimeoutEnabled = true
		options.MaxRetries = -1
		return redis.NewClient(options), nil
	}
	if input.Type != "redis" || input.URL != "" || input.DB < 0 || input.PoolSize < 0 ||
		(input.Password == "****" || input.SentinelPassword == "****") {
		return nil, errors.New("Invalid Redis topology settings")
	}
	var tlsConfig *tls.Config
	if input.SSL {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	switch topology {
	case "cluster":
		if len(input.StartupNodes) == 0 || len(input.StartupNodes) > 32 || input.DB != 0 {
			return nil, errors.New("Supply Redis Cluster startup nodes and use database zero")
		}
		addresses := make([]string, 0, len(input.StartupNodes))
		for _, node := range input.StartupNodes {
			address, err := cacheNodeAddress(node.Host, node.Port)
			if err != nil {
				return nil, err
			}
			addresses = append(addresses, address)
		}
		return redis.NewClusterClient(&redis.ClusterOptions{
			Addrs: addresses, Username: input.Username, Password: input.Password, TLSConfig: tlsConfig,
			PoolSize: input.PoolSize, ContextTimeoutEnabled: true, MaxRetries: -1,
		}), nil
	case "sentinel":
		if len(input.SentinelNodes) == 0 || len(input.SentinelNodes) > 32 || input.ServiceName == "" {
			return nil, errors.New("Supply Redis Sentinel nodes and a master service name")
		}
		addresses := make([]string, 0, len(input.SentinelNodes))
		for _, node := range input.SentinelNodes {
			var host string
			if len(node) != 2 || json.Unmarshal(node[0], &host) != nil {
				return nil, errors.New("Invalid Redis Sentinel node")
			}
			address, err := cacheNodeAddress(host, node[1])
			if err != nil {
				return nil, err
			}
			addresses = append(addresses, address)
		}
		return redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName: input.ServiceName, SentinelAddrs: addresses, SentinelPassword: input.SentinelPassword,
			Username: input.Username, Password: input.Password, DB: input.DB, TLSConfig: tlsConfig,
			PoolSize: input.PoolSize, ContextTimeoutEnabled: true, MaxRetries: -1,
		}), nil
	default:
		return nil, errors.New("Unsupported Redis topology")
	}
}

