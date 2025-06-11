package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
)

type RPCRequest struct {
	ID     interface{} `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

type RPCResponse struct {
	ID     interface{} `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  interface{} `json:"error,omitempty"`
}

type Config struct {
	Debug         bool
	CacheTTL      time.Duration
	Port          string
	RPCNodes      []string
	DirectMethods []string
}

type Server struct {
	config *Config
	cache  *cache.Cache
	client *http.Client
}

func NewServer(config *Config) *Server {
	return &Server{
		config: config,
		cache:  cache.New(config.CacheTTL, config.CacheTTL*2),
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func loadConfig() *Config {
	debug := os.Getenv("DEBUG") == "true"

	cacheTTL := time.Minute
	if ttlStr := os.Getenv("CACHE_TIME"); ttlStr != "" {
		if ttl, err := strconv.Atoi(ttlStr); err == nil {
			cacheTTL = time.Duration(ttl) * time.Minute
		}
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8545"
	}

	rpcNodesStr := os.Getenv("RPC_NODES")
	if rpcNodesStr == "" {
		rpcNodesStr = os.Getenv("ETHEREUM_RPC_NODE")
	}

	var rpcNodes []string
	if rpcNodesStr != "" {
		rpcNodes = strings.Split(rpcNodesStr, ",")
		for i, node := range rpcNodes {
			rpcNodes[i] = strings.TrimSpace(node)
		}
	}

	directMethods := []string{"eth_sendRawTransaction", "eth_call"}

	return &Config{
		Debug:         debug,
		CacheTTL:      cacheTTL,
		Port:          port,
		RPCNodes:      rpcNodes,
		DirectMethods: directMethods,
	}
}

func (s *Server) stripTrailingSlash(url string) string {
	return strings.TrimSuffix(url, "/")
}

func (s *Server) shouldUseCache(body interface{}) bool {
	if _, isArray := body.([]interface{}); isArray {
		if s.config.Debug {
			log.Println("cache disabled for batch request")
		}
		return false
	}

	bodyBytes, _ := json.Marshal(body)
	var req RPCRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		return false
	}

	for _, method := range s.config.DirectMethods {
		if req.Method == method {
			if s.config.Debug {
				log.Printf("cache disabled for %s", req.Method)
			}
			return false
		}
	}

	return true
}

func (s *Server) generateCacheKey(url string, body interface{}) string {
	bodyBytes, _ := json.Marshal(body)
	var req RPCRequest
	json.Unmarshal(bodyBytes, &req)

	content := url + req.Method + string(bodyBytes)
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:])
}

func (s *Server) makeRPCRequest(rpcNode, url string, method string, body []byte, headers http.Header) (*http.Response, error) {
	fullURL := rpcNode + url

	req, err := http.NewRequest(method, fullURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}

	for key, values := range headers {
		if strings.ToLower(key) != "host" {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	}

	return s.client.Do(req)
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	url := s.stripTrailingSlash(r.URL.Path)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	if s.config.Debug {
		log.Printf("request: %s", string(body))
	}

	var parsedBody interface{}
	if err := json.Unmarshal(body, &parsedBody); err != nil {
		stats := map[string]interface{}{
			"items": s.cache.ItemCount(),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
		return
	}

	if !s.isValidRPCRequest(parsedBody) {
		stats := map[string]interface{}{
			"items": s.cache.ItemCount(),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats)
		return
	}

	useCache := s.shouldUseCache(parsedBody)
	var requestID interface{}

	if reqMap, ok := parsedBody.(map[string]interface{}); ok {
		requestID = reqMap["id"]
	}

	var cacheKey string
	if useCache {
		cacheKey = s.generateCacheKey(url, parsedBody)
		if cached, found := s.cache.Get(cacheKey); found {
			if s.config.Debug {
				log.Printf("found cache %s", cacheKey)
			}

			if cachedResponse, ok := cached.(map[string]interface{}); ok {
				cachedResponse["id"] = requestID
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(cachedResponse)
				return
			}
		}
	}

	var lastErr error
	for i, rpcNode := range s.config.RPCNodes {
		resp, err := s.makeRPCRequest(rpcNode, url, r.Method, body, r.Header)
		if err != nil {
			lastErr = err
			if s.config.Debug {
				log.Printf("RPC node %d failed: %v", i+1, err)
			}
			continue
		}

		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("RPC node returned %d status", resp.StatusCode)
			if s.config.Debug {
				log.Printf("RPC node %d returned %d status", i+1, resp.StatusCode)
			}
			continue
		}

		if resp.StatusCode == http.StatusOK && useCache {
			var responseData map[string]interface{}
			if json.Unmarshal(respBody, &responseData) == nil {
				if s.config.Debug {
					log.Printf("caching %s", cacheKey)
				}
				s.cache.Set(cacheKey, responseData, cache.DefaultExpiration)
			}
		}

		for key, values := range resp.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}

		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	log.Printf("All RPC nodes failed. Last error: %v", lastErr)
	errorResponse := map[string]interface{}{
		"error": map[string]interface{}{
			"code":    500,
			"message": fmt.Sprintf("All RPC nodes failed: %v", lastErr),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(errorResponse)
}

func (s *Server) isValidRPCRequest(body interface{}) bool {
	if _, isArray := body.([]interface{}); isArray {
		return true
	}

	if reqMap, ok := body.(map[string]interface{}); ok {
		_, hasMethod := reqMap["method"]
		return hasMethod
	}

	return false
}

func (s *Server) enableCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	s.handleRequest(w, r)
}

func main() {
	config := loadConfig()

	if len(config.RPCNodes) == 0 {
		log.Fatal("No RPC nodes configured. Set RPC_NODES or ETHEREUM_RPC_NODE environment variable")
	}

	server := NewServer(config)

	http.HandleFunc("/", server.enableCORS)

	log.Printf("Server starting on port %s with %d RPC nodes", config.Port, len(config.RPCNodes))
	if config.Debug {
		log.Printf("RPC nodes: %v", config.RPCNodes)
	}

	if err := http.ListenAndServe(":"+config.Port, nil); err != nil {
		log.Fatal("Server failed to start:", err)
	}
}
