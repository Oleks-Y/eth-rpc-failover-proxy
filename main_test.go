package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

type TestRPCRequest struct {
	ID     int         `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

type TestRPCResponse struct {
	ID     int         `json:"id"`
	Result interface{} `json:"result,omitempty"`
	Error  interface{} `json:"error,omitempty"`
}

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
		ID:     1,
		Method: "web3_clientVersion",
		Params: []interface{}{},
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

	assert.Equal(t, 200, recorder.statusCode)

	var response TestRPCResponse
	err = json.Unmarshal(recorder.body.Bytes(), &response)
	require.NoError(t, err)

	assert.Equal(t, 1, response.ID)
	assert.NotNil(t, response.Result)
	fmt.Println("Response:", response.Result)
	assert.Contains(t, strings.ToLower(response.Result.(string)), "anvil")
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
		ID:     1,
		Method: "web3_clientVersion",
		Params: []interface{}{},
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
		ID:     1,
		Method: "web3_clientVersion",
		Params: []interface{}{},
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
			ID:     1,
			Method: "web3_clientVersion",
			Params: []interface{}{},
		},
		{
			ID:     2,
			Method: "net_version",
			Params: []interface{}{},
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
