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
	"sync"
	"time"

	"github.com/gorilla/websocket"
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
	WSNodes       []string
	DirectMethods []string
}

type Server struct {
	config   *Config
	cache    *cache.Cache
	client   *http.Client
	upgrader websocket.Upgrader
}

// WSConnection manages a WebSocket connection with failover support
type WSConnection struct {
	server        *Server
	clientConn    *websocket.Conn
	backendConn   *websocket.Conn
	wsNodes       []string
	currentNode   int
	subscriptions map[interface{}]RPCRequest // Track active subscriptions by request ID
	subMutex      sync.RWMutex
	done          chan struct{}
	reconnecting  bool
	reconnectMux  sync.Mutex
	path          string
	debug         bool
}

func NewServer(config *Config) *Server {
	return &Server{
		config: config,
		cache:  cache.New(config.CacheTTL, config.CacheTTL*2),
		client: &http.Client{Timeout: 30 * time.Second},
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for simplicity
			},
			Subprotocols: []string{"wamp"}, // Common WebSocket subprotocol for RPC
		},
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
	var wsNodes []string

	if rpcNodesStr != "" {
		rpcNodes = strings.Split(rpcNodesStr, ",")
		for i, node := range rpcNodes {
			rpcNodes[i] = strings.TrimSpace(node)
		}
	}

	// Allow separate WebSocket node configuration
	wsNodesStr := os.Getenv("WS_NODES")
	if wsNodesStr != "" {
		customWSNodes := strings.Split(wsNodesStr, ",")
		for _, node := range customWSNodes {
			wsNodes = append(wsNodes, strings.TrimSpace(node))
		}
	}

	directMethods := []string{"eth_sendRawTransaction", "eth_call"}

	return &Config{
		Debug:         debug,
		CacheTTL:      cacheTTL,
		Port:          port,
		RPCNodes:      rpcNodes,
		WSNodes:       wsNodes,
		DirectMethods: directMethods,
	}
}

func convertHTTPToWebSocket(httpURL string) string {
	if strings.HasPrefix(httpURL, "http://") {
		return strings.Replace(httpURL, "http://", "ws://", 1)
	} else if strings.HasPrefix(httpURL, "https://") {
		return strings.Replace(httpURL, "https://", "wss://", 1)
	}
	return ""
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

func (s *Server) isWebSocketRequest(r *http.Request) bool {
	return strings.ToLower(r.Header.Get("Upgrade")) == "websocket"
}

func (s *Server) handleWebSocketRequest(w http.ResponseWriter, r *http.Request) {
	if len(s.config.WSNodes) == 0 {
		http.Error(w, "No WebSocket nodes configured", http.StatusServiceUnavailable)
		return
	}

	// Upgrade client connection to WebSocket
	clientConn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade client connection: %v", err)
		return
	}

	if s.config.Debug {
		log.Printf("WebSocket connection established from %s", r.RemoteAddr)
	}

	// Create WSConnection with failover support
	wsConn := &WSConnection{
		server:        s,
		clientConn:    clientConn,
		wsNodes:       s.config.WSNodes,
		currentNode:   0,
		subscriptions: make(map[interface{}]RPCRequest),
		done:          make(chan struct{}),
		path:          s.stripTrailingSlash(r.URL.Path),
		debug:         s.config.Debug,
	}

	// Initial connection to backend
	if err := wsConn.connectToBackend(); err != nil {
		log.Printf("Failed to connect to any WebSocket node: %v", err)
		clientConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "No backend available"))
		clientConn.Close()
		return
	}

	// Start proxying with failover support
	wsConn.startProxying()
}

func (ws *WSConnection) connectToBackend() error {
	for i := 0; i < len(ws.wsNodes); i++ {
		nodeIndex := (ws.currentNode + i) % len(ws.wsNodes)
		wsURL := ws.wsNodes[nodeIndex] + ws.path

		dialer := websocket.Dialer{
			Proxy:            http.ProxyFromEnvironment,
			HandshakeTimeout: 45 * time.Second,
		}

		conn, _, err := dialer.Dial(wsURL, nil)
		if err != nil {
			if ws.debug {
				log.Printf("Failed to connect to WebSocket node %d (%s): %v", nodeIndex+1, wsURL, err)
			}
			continue
		}

		ws.backendConn = conn
		ws.currentNode = nodeIndex
		if ws.debug {
			log.Printf("Connected to backend WebSocket node %d: %s", nodeIndex+1, wsURL)
		}
		return nil
	}

	return fmt.Errorf("all WebSocket nodes failed")
}

func (ws *WSConnection) startProxying() {
	defer ws.clientConn.Close()
	defer func() {
		if ws.backendConn != nil {
			ws.backendConn.Close()
		}
	}()

	// Client to backend
	go ws.clientToBackend()

	// Backend to client
	go ws.backendToClient()

	// Wait for completion
	<-ws.done
	if ws.debug {
		log.Printf("WebSocket proxy connection closed")
	}
}

func (ws *WSConnection) clientToBackend() {
	defer func() {
		ws.done <- struct{}{}
	}()

	for {
		select {
		case <-ws.done:
			return
		default:
		}

		messageType, message, err := ws.clientConn.ReadMessage()
		if err != nil {
			if ws.debug {
				log.Printf("Error reading from client: %v", err)
			}
			return
		}

		// Parse and track subscriptions
		ws.trackSubscription(message)

		if ws.debug {
			log.Printf("Client -> Backend: %s", string(message))
		}

		// Try to send to backend with failover
		if err := ws.sendToBackendWithFailover(messageType, message); err != nil {
			if ws.debug {
				log.Printf("Failed to send to any backend: %v", err)
			}
			return
		}
	}
}

func (ws *WSConnection) backendToClient() {
	defer func() {
		ws.done <- struct{}{}
	}()

	for {
		select {
		case <-ws.done:
			return
		default:
		}

		if ws.backendConn == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		messageType, message, err := ws.backendConn.ReadMessage()
		if err != nil {
			if ws.debug {
				log.Printf("Error reading from backend: %v", err)
			}

			// Attempt reconnection
			if ws.handleBackendDisconnection() {
				continue // Retry reading from new connection
			}
			return
		}

		if ws.debug {
			log.Printf("Backend -> Client: %s", string(message))
		}

		err = ws.clientConn.WriteMessage(messageType, message)
		if err != nil {
			if ws.debug {
				log.Printf("Error writing to client: %v", err)
			}
			return
		}
	}
}

func (ws *WSConnection) trackSubscription(message []byte) {
	var req RPCRequest
	if err := json.Unmarshal(message, &req); err != nil {
		return
	}

	ws.subMutex.Lock()
	defer ws.subMutex.Unlock()

	// Track subscription requests
	if req.Method == "eth_subscribe" {
		ws.subscriptions[req.ID] = req
		if ws.debug {
			log.Printf("Tracking subscription with ID %v", req.ID)
		}
	}

	// Remove subscription on unsubscribe
	if req.Method == "eth_unsubscribe" {
		if params, ok := req.Params.([]interface{}); ok && len(params) > 0 {
			subscriptionID := params[0]
			// Find and remove the subscription by subscription ID
			for reqID, sub := range ws.subscriptions {
				if subResp, ok := sub.Params.([]interface{}); ok && len(subResp) > 0 {
					if subResp[0] == subscriptionID {
						delete(ws.subscriptions, reqID)
						if ws.debug {
							log.Printf("Removed subscription with ID %v", reqID)
						}
						break
					}
				}
			}
		}
	}
}

func (ws *WSConnection) sendToBackendWithFailover(messageType int, message []byte) error {
	maxRetries := len(ws.wsNodes)

	for attempt := 0; attempt < maxRetries; attempt++ {
		if ws.backendConn == nil {
			if err := ws.connectToBackend(); err != nil {
				continue
			}
		}

		err := ws.backendConn.WriteMessage(messageType, message)
		if err == nil {
			return nil // Success
		}

		if ws.debug {
			log.Printf("Error writing to backend (attempt %d): %v", attempt+1, err)
		}

		// Connection failed, try next node
		if ws.backendConn != nil {
			ws.backendConn.Close()
			ws.backendConn = nil
		}

		ws.currentNode = (ws.currentNode + 1) % len(ws.wsNodes)
	}

	return fmt.Errorf("failed to send message to any backend after %d attempts", maxRetries)
}

func (ws *WSConnection) handleBackendDisconnection() bool {
	ws.reconnectMux.Lock()
	defer ws.reconnectMux.Unlock()

	if ws.reconnecting {
		return false
	}
	ws.reconnecting = true
	defer func() { ws.reconnecting = false }()

	if ws.debug {
		log.Printf("Backend disconnected, attempting reconnection...")
	}

	// Close current connection
	if ws.backendConn != nil {
		ws.backendConn.Close()
		ws.backendConn = nil
	}

	// Try to reconnect to next node
	ws.currentNode = (ws.currentNode + 1) % len(ws.wsNodes)

	if err := ws.connectToBackend(); err != nil {
		if ws.debug {
			log.Printf("Reconnection failed: %v", err)
		}
		return false
	}

	// Re-establish subscriptions
	if err := ws.reestablishSubscriptions(); err != nil {
		if ws.debug {
			log.Printf("Failed to re-establish subscriptions: %v", err)
		}
		return false
	}

	if ws.debug {
		log.Printf("Successfully reconnected and re-established %d subscriptions", len(ws.subscriptions))
	}

	return true
}

func (ws *WSConnection) reestablishSubscriptions() error {
	ws.subMutex.RLock()
	subscriptions := make(map[interface{}]RPCRequest)
	for id, sub := range ws.subscriptions {
		subscriptions[id] = sub
	}
	ws.subMutex.RUnlock()

	for _, sub := range subscriptions {
		message, err := json.Marshal(sub)
		if err != nil {
			continue
		}

		if ws.debug {
			log.Printf("Re-establishing subscription: %s", string(message))
		}

		err = ws.backendConn.WriteMessage(websocket.TextMessage, message)
		if err != nil {
			return fmt.Errorf("failed to re-establish subscription %v: %v", sub.ID, err)
		}

		// Small delay between subscription requests
		time.Sleep(10 * time.Millisecond)
	}

	return nil
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

		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnauthorized {
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

	fmt.Println("CORS headers set")
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Check if this is a WebSocket upgrade request
	if s.isWebSocketRequest(r) {
		s.handleWebSocketRequest(w, r)
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

	log.Printf("Server starting on port %s with %d HTTP RPC nodes and %d WebSocket nodes",
		config.Port, len(config.RPCNodes), len(config.WSNodes))
	log.Printf("Note: For WSS support, configure TLS_CERT_FILE and TLS_KEY_FILE")

	if config.Debug {
		log.Printf("HTTP RPC nodes: %v", config.RPCNodes)
		log.Printf("WebSocket nodes: %v", config.WSNodes)
	}

	if err := http.ListenAndServe(":"+config.Port, nil); err != nil {
		log.Fatal("Server failed to start:", err)
	}
}
