package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Existing test types and helper functions
type TestRPCRequest struct {
	ID      interface{} `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	JSONRPC string      `json:"jsonrpc"`
}

type TestRPCResponse struct {
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   interface{} `json:"error,omitempty"`
	JSONRPC string      `json:"jsonrpc,omitempty"`
}

// WebSocket test server for mocking backend WebSocket nodes
type MockWSServer struct {
	server   *httptest.Server
	upgrader websocket.Upgrader
	clients  map[*websocket.Conn]bool
	mu       sync.RWMutex
	fail     bool // Controls whether server should fail
	messages [][]byte // Store received messages for verification
}

func NewMockWSServer() *MockWSServer {
	mock := &MockWSServer{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		clients:  make(map[*websocket.Conn]bool),
		messages: make([][]byte, 0),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", mock.handleWebSocket)
	mock.server = httptest.NewServer(mux)

	return mock
}

func (m *MockWSServer) URL() string {
	return "ws" + strings.TrimPrefix(m.server.URL, "http")
}

func (m *MockWSServer) Close() {
	m.server.Close()
}

func (m *MockWSServer) SetFail(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = fail
}

func (m *MockWSServer) GetMessages() [][]byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	messages := make([][]byte, len(m.messages))
	copy(messages, m.messages)
	return messages
}

func (m *MockWSServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	shouldFail := m.fail
	m.mu.RUnlock()

	if shouldFail {
		http.Error(w, "Server unavailable", http.StatusServiceUnavailable)
		return
	}

	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	m.mu.Lock()
	m.clients[conn] = true
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.clients, conn)
		m.mu.Unlock()
	}()

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			break
		}

		// Store message for verification
		m.mu.Lock()
		m.messages = append(m.messages, message)
		m.mu.Unlock()

		// Handle subscription requests
		var req TestRPCRequest
		if json.Unmarshal(message, &req) == nil {
			if req.Method == "eth_subscribe" {
				// Send subscription response
				response := TestRPCResponse{
					ID:      req.ID,
					Result:  "0x1234567890abcdef",
					JSONRPC: "2.0",
				}
				respBytes, _ := json.Marshal(response)
				conn.WriteMessage(messageType, respBytes)

				// Send a subscription notification after a delay
				go func() {
					time.Sleep(100 * time.Millisecond)
					notification := map[string]interface{}{
						"jsonrpc": "2.0",
						"method":  "eth_subscription",
						"params": map[string]interface{}{
							"subscription": "0x1234567890abcdef",
							"result": map[string]interface{}{
								"blockNumber": "0x1234",
								"blockHash":   "0xabcdef",
							},
						},
					}
					notifBytes, _ := json.Marshal(notification)
					conn.WriteMessage(websocket.TextMessage, notifBytes)
				}()
			} else {
				// Echo other requests
				response := TestRPCResponse{
					ID:      req.ID,
					Result:  "mock_result",
					JSONRPC: "2.0",
				}
				respBytes, _ := json.Marshal(response)
				conn.WriteMessage(messageType, respBytes)
			}
		}
	}
}

func TestWebSocketProxyBasicConnection(t *testing.T) {
	// Create mock backend WebSocket server
	mockServer := NewMockWSServer()
	defer mockServer.Close()

	// Create proxy server configuration
	config := &Config{
		Debug:    true,
		CacheTTL: time.Minute,
		Port:     "0",
		WSNodes:  []string{mockServer.URL()},
	}

	server := NewServer(config)

	// Create test HTTP server for the proxy
	mux := http.NewServeMux()
	mux.HandleFunc("/", server.enableCORS)
	testServer := httptest.NewServer(mux)
	defer testServer.Close()

	// Convert HTTP URL to WebSocket URL
	wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http")

	// Create WebSocket client connection to proxy
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	// Send a test message
	testReq := TestRPCRequest{
		ID:      1,
		Method:  "eth_blockNumber",
		Params:  []interface{}{},
		JSONRPC: "2.0",
	}

	reqBytes, err := json.Marshal(testReq)
	require.NoError(t, err)

	err = conn.WriteMessage(websocket.TextMessage, reqBytes)
	require.NoError(t, err)

	// Read response
	messageType, message, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.TextMessage, messageType)

	var response TestRPCResponse
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, 1, response.ID)
	assert.Equal(t, "mock_result", response.Result)

	// Verify message was received by backend
	messages := mockServer.GetMessages()
	assert.Len(t, messages, 1)

	var receivedReq TestRPCRequest
	err = json.Unmarshal(messages[0], &receivedReq)
	require.NoError(t, err)
	assert.Equal(t, "eth_blockNumber", receivedReq.Method)
}

func TestWebSocketProxyFailover(t *testing.T) {
	// Create two mock backend servers
	failingServer := NewMockWSServer()
	failingServer.SetFail(true)
	defer failingServer.Close()

	workingServer := NewMockWSServer()
	defer workingServer.Close()

	// Create proxy server with failing server first
	config := &Config{
		Debug:    true,
		CacheTTL: time.Minute,
		Port:     "0",
		WSNodes:  []string{failingServer.URL(), workingServer.URL()},
	}

	server := NewServer(config)

	// Create test HTTP server for the proxy
	mux := http.NewServeMux()
	mux.HandleFunc("/", server.enableCORS)
	testServer := httptest.NewServer(mux)
	defer testServer.Close()

	// Convert HTTP URL to WebSocket URL
	wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http")

	// Create WebSocket client connection to proxy
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	// Send a test message - should connect to working server after failing server fails
	testReq := TestRPCRequest{
		ID:      1,
		Method:  "eth_blockNumber",
		Params:  []interface{}{},
		JSONRPC: "2.0",
	}

	reqBytes, err := json.Marshal(testReq)
	require.NoError(t, err)

	err = conn.WriteMessage(websocket.TextMessage, reqBytes)
	require.NoError(t, err)

	// Read response
	messageType, message, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.TextMessage, messageType)

	var response TestRPCResponse
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, 1, response.ID)

	// Verify message was received by working server
	messages := workingServer.GetMessages()
	assert.Len(t, messages, 1)
}

func TestWebSocketProxySubscription(t *testing.T) {
	// Create mock backend WebSocket server
	mockServer := NewMockWSServer()
	defer mockServer.Close()

	// Create proxy server configuration
	config := &Config{
		Debug:    true,
		CacheTTL: time.Minute,
		Port:     "0",
		WSNodes:  []string{mockServer.URL()},
	}

	server := NewServer(config)

	// Create test HTTP server for the proxy
	mux := http.NewServeMux()
	mux.HandleFunc("/", server.enableCORS)
	testServer := httptest.NewServer(mux)
	defer testServer.Close()

	// Convert HTTP URL to WebSocket URL
	wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http")

	// Create WebSocket client connection to proxy
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	// Send subscription request
	subReq := TestRPCRequest{
		ID:      1,
		Method:  "eth_subscribe",
		Params:  []interface{}{"newHeads"},
		JSONRPC: "2.0",
	}

	reqBytes, err := json.Marshal(subReq)
	require.NoError(t, err)

	err = conn.WriteMessage(websocket.TextMessage, reqBytes)
	require.NoError(t, err)

	// Read subscription response
	messageType, message, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.TextMessage, messageType)

	var subResponse TestRPCResponse
	err = json.Unmarshal(message, &subResponse)
	require.NoError(t, err)
	assert.Equal(t, 1, subResponse.ID)
	assert.NotNil(t, subResponse.Result)

	// Read subscription notification
	messageType, message, err = conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.TextMessage, messageType)

	var notification map[string]interface{}
	err = json.Unmarshal(message, &notification)
	require.NoError(t, err)
	assert.Equal(t, "eth_subscription", notification["method"])
	assert.Contains(t, notification, "params")
}

func TestWebSocketProxySubscriptionFailover(t *testing.T) {
	// Create two mock backend servers
	server1 := NewMockWSServer()
	defer server1.Close()

	server2 := NewMockWSServer()
	defer server2.Close()

	// Create proxy server configuration
	config := &Config{
		Debug:    true,
		CacheTTL: time.Minute,
		Port:     "0",
		WSNodes:  []string{server1.URL(), server2.URL()},
	}

	server := NewServer(config)

	// Create test HTTP server for the proxy
	mux := http.NewServeMux()
	mux.HandleFunc("/", server.enableCORS)
	testServer := httptest.NewServer(mux)
	defer testServer.Close()

	// Convert HTTP URL to WebSocket URL
	wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http")

	// Create WebSocket client connection to proxy
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	// Send subscription request
	subReq := TestRPCRequest{
		ID:      1,
		Method:  "eth_subscribe",
		Params:  []interface{}{"newHeads"},
		JSONRPC: "2.0",
	}

	reqBytes, err := json.Marshal(subReq)
	require.NoError(t, err)

	err = conn.WriteMessage(websocket.TextMessage, reqBytes)
	require.NoError(t, err)

	// Read subscription response
	messageType, message, err := conn.ReadMessage()
	require.NoError(t, err)
	require.NotNil(t, messageType)
	var subResponse TestRPCResponse
	err = json.Unmarshal(message, &subResponse)
	require.NoError(t, err)

	// Read notification from first server
	messageType, message, err = conn.ReadMessage()
	require.NotNil(t, messageType)
	require.NoError(t, err)

	// Now make first server fail
	server1.SetFail(true)
	server1.Close() // Force disconnection

	// Wait a bit for failover to occur
	time.Sleep(500 * time.Millisecond)

	// Send another message - should work after failover
	testReq := TestRPCRequest{
		ID:      2,
		Method:  "eth_blockNumber",
		Params:  []interface{}{},
		JSONRPC: "2.0",
	}

	reqBytes2, err := json.Marshal(testReq)
	require.NoError(t, err)

	err = conn.WriteMessage(websocket.TextMessage, reqBytes2)
	require.NoError(t, err)

	// Should receive response from second server
	messageType, message, err = conn.ReadMessage()
	require.NoError(t, err)

	var response TestRPCResponse
	err = json.Unmarshal(message, &response)
	require.NoError(t, err)
	assert.Equal(t, 2, response.ID)

	// Verify second server received messages (original subscription + new request)
	messages := server2.GetMessages()
	assert.GreaterOrEqual(t, len(messages), 1) // At least the re-established subscription
}

func TestWebSocketProxyAllNodesFail(t *testing.T) {
	// Create failing mock servers
	server1 := NewMockWSServer()
	server1.SetFail(true)
	defer server1.Close()

	server2 := NewMockWSServer()
	server2.SetFail(true)
	defer server2.Close()

	// Create proxy server configuration
	config := &Config{
		Debug:    true,
		CacheTTL: time.Minute,
		Port:     "0",
		WSNodes:  []string{server1.URL(), server2.URL()},
	}

	server := NewServer(config)

	// Create test HTTP server for the proxy
	mux := http.NewServeMux()
	mux.HandleFunc("/", server.enableCORS)
	testServer := httptest.NewServer(mux)
	defer testServer.Close()

	// Convert HTTP URL to WebSocket URL
	wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http")

	// Try to create WebSocket client connection to proxy
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial(wsURL, nil)

	// Connection should fail or close immediately
	if err == nil {
		defer conn.Close()
		// If connection succeeds, it should close quickly due to backend failures
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, readErr := conn.ReadMessage()
		assert.Error(t, readErr) // Should get an error due to connection closure
	} else {
		// Connection should fail with appropriate error
		assert.Error(t, err)
		if resp != nil {
			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		}
	}
}

func TestWebSocketProxyUnsubscribe(t *testing.T) {
	// Create mock backend WebSocket server
	mockServer := NewMockWSServer()
	defer mockServer.Close()

	// Create proxy server configuration
	config := &Config{
		Debug:    true,
		CacheTTL: time.Minute,
		Port:     "0",
		WSNodes:  []string{mockServer.URL()},
	}

	server := NewServer(config)

	// Create test HTTP server for the proxy
	mux := http.NewServeMux()
	mux.HandleFunc("/", server.enableCORS)
	testServer := httptest.NewServer(mux)
	defer testServer.Close()

	// Convert HTTP URL to WebSocket URL
	wsURL := "ws" + strings.TrimPrefix(testServer.URL, "http")

	// Create WebSocket client connection to proxy
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	// Send subscription request
	subReq := TestRPCRequest{
		ID:      1,
		Method:  "eth_subscribe",
		Params:  []interface{}{"newHeads"},
		JSONRPC: "2.0",
	}

	reqBytes, err := json.Marshal(subReq)
	require.NoError(t, err)

	err = conn.WriteMessage(websocket.TextMessage, reqBytes)
	require.NoError(t, err)

	// Read subscription response
	messageType, message, err := conn.ReadMessage()
	require.NoError(t, err)
	require.NotNil(t, messageType)

	var subResponse TestRPCResponse
	err = json.Unmarshal(message, &subResponse)
	require.NoError(t, err)
	subscriptionID := subResponse.Result

	// Send unsubscribe request
	unsubReq := TestRPCRequest{
		ID:      2,
		Method:  "eth_unsubscribe",
		Params:  []interface{}{subscriptionID},
		JSONRPC: "2.0",
	}

	unsubBytes, err := json.Marshal(unsubReq)
	require.NoError(t, err)

	err = conn.WriteMessage(websocket.TextMessage, unsubBytes)
	require.NoError(t, err)

	// Read unsubscribe response
	messageType, message, err = conn.ReadMessage()
	require.NoError(t, err)

	var unsubResponse TestRPCResponse
	err = json.Unmarshal(message, &unsubResponse)
	require.NoError(t, err)
	assert.Equal(t, 2, unsubResponse.ID)

	// Verify both subscription and unsubscription messages were sent to backend
	messages := mockServer.GetMessages()
	assert.GreaterOrEqual(t, len(messages), 2)
}

// Existing test helper functions and types...

func startAnvilContainer(ctx context.Context, t *testing.T) (testcontainers.Container, string) {
	req := testcontainers.ContainerRequest{
		Image:        "ghcr.io/foundry-rs/foundry:latest",
		Cmd:          []string{"anvil --host=0.0.0.0 --port 8545"},
		ExposedPorts: []string{"8545"},
		WaitingFor:   wait.ForListeningPort("8545").WithStartupTimeout(15 * time.Second),
		Env: map[string]string{
			"FOUNDRY_DISABLE_NIGHTLY_WARNING": "true",
		},
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	host, err := container.Host(ctx)
	require.NoError(t, err)

	port, err := container.MappedPort(ctx, "8545")
	require.NoError(t, err)

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())
	fmt.Println("Anvil endpoint:", endpoint)
	time.Sleep(5 * time.Second)
	waitForRPC(t, endpoint)

	return container, endpoint
}

func startFailingRPCContainer(ctx context.Context, t *testing.T) (testcontainers.Container, string) {
	req := testcontainers.ContainerRequest{
		Image:        "nginx:alpine",
		ExposedPorts: []string{"80/tcp"},
		Files: []testcontainers.ContainerFile{
			{
				HostFilePath:      createTempFile(t, "nginx.conf", `
events {
    worker_connections 1024;
}
http {
    server {
        listen 80;
        location / {
            return 500 '{"error":{"code":-32603,"message":"Internal error"}}';
            add_header Content-Type application/json;
        }
    }
}
`),
				ContainerFilePath: "/etc/nginx/nginx.conf",
				FileMode:          0644,
			},
		},
		WaitingFor: wait.ForExposedPort().WithStartupTimeout(30 * time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	host, err := container.Host(ctx)
	require.NoError(t, err)

	port, err := container.MappedPort(ctx, "80")
	require.NoError(t, err)

	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())

	time.Sleep(2 * time.Second)
	fmt.Println(endpoint)
	return container, endpoint
}

func createTempFile(t *testing.T, name, content string) string {
	tmpFile, err := os.CreateTemp("", name)
	require.NoError(t, err)

	_, err = tmpFile.WriteString(content)
	require.NoError(t, err)

	err = tmpFile.Close()
	require.NoError(t, err)

	t.Cleanup(func() {
		os.Remove(tmpFile.Name())
	})

	return tmpFile.Name()
}

func waitForEndpoint(t *testing.T, endpoint string, expectStatus int) {
	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < 30; i++ {
		resp, err := client.Get(endpoint)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == expectStatus {
				return
			}
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("Endpoint %s did not become available with status %d", endpoint, expectStatus)
}

func waitForRPC(t *testing.T, endpoint string) {
	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < 60; i++ {
		req := TestRPCRequest{
			ID:     1,
			Method: "web3_clientVersion",
			Params: []interface{}{},
		}

		body, _ := json.Marshal(req)
		resp, err := client.Post(endpoint, "application/json", bytes.NewBuffer(body))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 500 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("RPC endpoint did not become available")
}

// Existing RPC tests...

func TestRPCProxyFailover(t *testing.T) {
	ctx := context.Background()

	failingContainer, failingEndpoint := startFailingRPCContainer(ctx, t)
	defer failingContainer.Terminate(ctx)

	healthyContainer, healthyEndpoint := startAnvilContainer(ctx, t)
	defer healthyContainer.Terminate(ctx)

	config := &Config{
		Debug:         true,
		CacheTTL:      time.Minute,
		Port:          "0",
		RPCNodes:      []string{failingEndpoint, healthyEndpoint},
		DirectMethods: []string{"eth_sendRawTransaction", "eth_call"},
	}

	server := NewServer(config)

	req := TestRPCRequest{
		ID:      1,
		Method:  "web3_clientVersion",
		Params:  []interface{}{},
		JSONRPC: "2.0",
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq, err := http.NewRequest("POST", "/", bytes.NewBuffer(body))
	httpReq.Header.Set("Content-Type", "application/json")

	recorder := &testResponseWriter{
		header: make(http.Header),
		body:   &bytes.Buffer{},
	}

	server.handleRequest(recorder, httpReq)

	assert.Equal(t, 200, recorder.statusCode)

	var response TestRPCResponse
	err = json.Unmarshal(recorder.body.Bytes(), &response)
	require.NoError(t, err)

	assert.Equal(t, 1, response.ID)

	if response.Result == nil {
		t.Log(response.Error)
	}
	require.NoError(t, err)

	if response.Result != nil {
		if resultStr, ok := response.Result.(string); ok {
			fmt.Println("Response:", response.Result)
			assert.Contains(t, strings.ToLower(resultStr), "anvil")
		} else {
			t.Errorf("Expected result to be a string, got %T", response.Result)
		}
	}
}

func TestRPCProxyAllNodesFail(t *testing.T) {
	ctx := context.Background()

	failing1, endpoint1 := startFailingRPCContainer(ctx, t)
	defer failing1.Terminate(ctx)

	failing2, endpoint2 := startFailingRPCContainer(ctx, t)
	defer failing2.Terminate(ctx)

	config := &Config{
		Debug:         true,
		CacheTTL:      time.Minute,
		Port:          "0",
		RPCNodes:      []string{endpoint1, endpoint2},
		DirectMethods: []string{"eth_sendRawTransaction", "eth_call"},
	}

	server := NewServer(config)

	req := TestRPCRequest{
		ID:      1,
		Method:  "web3_clientVersion",
		Params:  []interface{}{},
		JSONRPC: "2.0",
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq, err := http.NewRequest("POST", "/", bytes.NewBuffer(body))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	recorder := &testResponseWriter{
		header: make(http.Header),
		body:   &bytes.Buffer{},
	}

	server.handleRequest(recorder, httpReq)

	assert.Equal(t, 500, recorder.statusCode)

	var response map[string]interface{}
	err = json.Unmarshal(recorder.body.Bytes(), &response)
	require.NoError(t, err)

	assert.Contains(t, response, "error")
}

func TestRPCProxyCaching(t *testing.T) {
	ctx := context.Background()

	container, endpoint := startAnvilContainer(ctx, t)
	defer container.Terminate(ctx)

	config := &Config{
		Debug:         true,
		CacheTTL:      time.Minute,
		Port:          "0",
		RPCNodes:      []string{endpoint},
		DirectMethods: []string{"eth_sendRawTransaction", "eth_call"},
	}

	server := NewServer(config)

	req := TestRPCRequest{
		ID:      1,
		Method:  "web3_clientVersion",
		Params:  []interface{}{},
		JSONRPC: "2.0",
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	httpReq1, err := http.NewRequest("POST", "/", bytes.NewBuffer(body))
	require.NoError(t, err)
	httpReq1.Header.Set("Content-Type", "application/json")

	recorder1 := &testResponseWriter{
		header: make(http.Header),
		body:   &bytes.Buffer{},
	}

	server.handleRequest(recorder1, httpReq1)
	assert.Equal(t, 200, recorder1.statusCode)

	initialCacheCount := server.cache.ItemCount()

	httpReq2, err := http.NewRequest("POST", "/", bytes.NewBuffer(body))
	require.NoError(t, err)
	httpReq2.Header.Set("Content-Type", "application/json")

	recorder2 := &testResponseWriter{
		header: make(http.Header),
		body:   &bytes.Buffer{},
	}

	server.handleRequest(recorder2, httpReq2)
	assert.Equal(t, 200, recorder2.statusCode)

	finalCacheCount := server.cache.ItemCount()
	assert.Equal(t, initialCacheCount, finalCacheCount)

	var response1, response2 TestRPCResponse
	json.Unmarshal(recorder1.body.Bytes(), &response1)
	json.Unmarshal(recorder2.body.Bytes(), &response2)

	assert.Equal(t, response1.Result, response2.Result)
}

func TestRPCProxyDirectMethodsNoCaching(t *testing.T) {
	ctx := context.Background()

	container, endpoint := startAnvilContainer(ctx, t)
	defer container.Terminate(ctx)

	config := &Config{
		Debug:         true,
		CacheTTL:      time.Minute,
		Port:          "0",
		RPCNodes:      []string{endpoint},
		DirectMethods: []string{"eth_sendRawTransaction", "eth_call"},
	}

	server := NewServer(config)

	req := TestRPCRequest{
		ID:     1,
		Method: "eth_call",
		Params: []interface{}{
			map[string]interface{}{
				"to":   "0x0000000000000000000000000000000000000000",
				"data": "0x",
			},
			"latest",
		},
		JSONRPC: "2.0",
	}

	body, err := json.Marshal(req)
	require.NoError(t, err)

	initialCacheCount := server.cache.ItemCount()

	httpReq, err := http.NewRequest("POST", "/", bytes.NewBuffer(body))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	recorder := &testResponseWriter{
		header: make(http.Header),
		body:   &bytes.Buffer{},
	}

	server.handleRequest(recorder, httpReq)
	assert.Equal(t, 200, recorder.statusCode)

	finalCacheCount := server.cache.ItemCount()
	assert.Equal(t, initialCacheCount, finalCacheCount)
}

func TestRPCProxyBatchRequest(t *testing.T) {
	ctx := context.Background()

	container, endpoint := startAnvilContainer(ctx, t)
	defer container.Terminate(ctx)

	config := &Config{
		Debug:         true,
		CacheTTL:      time.Minute,
		Port:          "0",
		RPCNodes:      []string{endpoint},
		DirectMethods: []string{"eth_sendRawTransaction", "eth_call"},
	}

	server := NewServer(config)

	batchReq := []TestRPCRequest{
		{
			ID:      1,
			Method:  "web3_clientVersion",
			Params:  []interface{}{},
			JSONRPC: "2.0",
		},
		{
			ID:      2,
			Method:  "net_version",
			Params:  []interface{}{},
			JSONRPC: "2.0",
		},
	}

	body, err := json.Marshal(batchReq)
	require.NoError(t, err)

	httpReq, err := http.NewRequest("POST", "/", bytes.NewBuffer(body))
	require.NoError(t, err)
	httpReq.Header.Set("Content-Type", "application/json")

	recorder := &testResponseWriter{
		header: make(http.Header),
		body:   &bytes.Buffer{},
	}

	server.handleRequest(recorder, httpReq)
	assert.Equal(t, 200, recorder.statusCode)

	var response []TestRPCResponse
	err = json.Unmarshal(recorder.body.Bytes(), &response)
	require.NoError(t, err)

	assert.Len(t, response, 2)
	assert.Equal(t, 1, response[0].ID)
	assert.Equal(t, 2, response[1].ID)
}

type testResponseWriter struct {
	header     http.Header
	body       *bytes.Buffer
	statusCode int
}

func (w *testResponseWriter) Header() http.Header {
	return w.header
}

func (w *testResponseWriter) Write(data []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = 200
	}
	return w.body.Write(data)
}

func (w *testResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}
