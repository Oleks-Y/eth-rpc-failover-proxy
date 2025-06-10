# RPC Proxy Server

A high-performance RPC proxy server written in Go that provides caching, failover, and load balancing for Ethereum RPC endpoints.

## Features

- **Multiple RPC Node Support**: Configure multiple RPC endpoints with automatic failover
- **Intelligent Caching**: In-memory caching with configurable TTL to reduce RPC calls
- **Failover Mechanism**: Automatically switches to healthy nodes when endpoints return 5xx errors
- **Direct Method Bypass**: Certain methods bypass caching for real-time data
- **Batch Request Support**: Handles both single and batch JSON-RPC requests
- **CORS Support**: Built-in Cross-Origin Resource Sharing support
- **Debug Mode**: Configurable logging for troubleshooting

## Quick Start

### Prerequisites

- Go 1.21 or later
- Docker (for running tests)

### Installation

```bash
git clone <repository>
cd rpc-proxy
go mod tidy
```

### Configuration

Set environment variables:

```bash
# Multiple RPC endpoints (comma-separated)
export ETHEREUM_RPC_NODES="https://eth-mainnet.g.alchemy.com/v2/YOUR_KEY,https://mainnet.infura.io/v3/YOUR_KEY"

# Or single endpoint (fallback)
export ETHEREUM_RPC_NODE="https://eth-mainnet.g.alchemy.com/v2/YOUR_KEY"

# Optional configuration
export PORT=8545                # Server port (default: 8545)
export CACHE_TIME=5             # Cache TTL in minutes (default: 1)
export DEBUG=true               # Enable debug logging (default: false)
```

### Running the Server

```bash
go run main.go
```

The server will start on the configured port and log the number of RPC nodes configured.

## Usage

### Single RPC Request

```bash
curl -X POST http://localhost:8545 \
  -H "Content-Type: application/json" \
  -d '{
    "id": 1,
    "method": "eth_blockNumber",
    "params": []
  }'
```

### Batch RPC Request

```bash
curl -X POST http://localhost:8545 \
  -H "Content-Type: application/json" \
  -d '[
    {
      "id": 1,
      "method": "eth_blockNumber",
      "params": []
    },
    {
      "id": 2,
      "method": "web3_clientVersion",
      "params": []
    }
  ]'
```

### Cache Statistics

```bash
curl http://localhost:8545
```

## Failover Behavior

The proxy tries RPC nodes in the configured order:

1. **Network Errors**: If a node is unreachable, immediately try the next node
2. **5xx Errors**: If a node returns a 5xx HTTP status code, treat as failure and try next node
3. **Success**: Return the first successful response
4. **All Failed**: Return error response if all nodes fail

## Caching Logic

### Cached Methods
- Most read-only methods (e.g., `eth_blockNumber`, `eth_getBalance`)
- Cache key includes URL path, method, and parameters

### Non-Cached Methods
- `eth_sendRawTransaction` - Transactions should not be cached
- `eth_call` - May return different results based on block state
- Batch requests - Too complex for reliable caching

### Cache Invalidation
- Time-based expiration (configurable TTL)
- No manual invalidation currently supported

## Testing

### Unit Tests

```bash
go test -v
```

### Integration Tests

Integration tests use testcontainers to spin up real Anvil (Ethereum) nodes and test failover scenarios.

**Prerequisites:**
- Docker running
- Internet connection (to pull container images)

```bash
# Run all integration tests
./test_runner.sh

# Run specific test
go test -v -run TestRPCProxyFailover
```

### Test Scenarios

- **Failover**: First node returns 5xx, second succeeds
- **All Nodes Fail**: All nodes return 5xx errors
- **Caching**: Verify cache behavior for cacheable methods
- **Direct Methods**: Confirm non-cacheable methods bypass cache
- **Batch Requests**: Test batch request handling

## Configuration Reference

| Environment Variable | Description | Default | Example |
|---------------------|-------------|---------|---------|
| `ETHEREUM_RPC_NODES` | Comma-separated RPC endpoints | - | `"https://rpc1.com,https://rpc2.com"` |
| `ETHEREUM_RPC_NODE` | Single RPC endpoint (fallback) | - | `"https://mainnet.infura.io/v3/KEY"` |
| `PORT` | Server port | `8545` | `3000` |
| `CACHE_TIME` | Cache TTL in minutes | `1` | `5` |
| `DEBUG` | Enable debug logging | `false` | `true` |

## Architecture

```
Client Request
      ↓
   CORS Handler
      ↓
  Cache Check
      ↓
 RPC Node 1 ──5xx──→ RPC Node 2 ──5xx──→ RPC Node N
      ↓                    ↓                    ↓
   Success              Success            All Failed
      ↓                    ↓                    ↓
  Cache Store         Cache Store         Error Response
      ↓                    ↓                    ↓
 Client Response     Client Response     Client Response
```

## Performance Considerations

- **Memory Usage**: Cache grows with unique requests; monitor in production
- **Request Latency**: First request to new endpoint may be slower due to caching
- **Failover Time**: Network timeouts add latency when nodes are down
- **Concurrent Requests**: Go's HTTP server handles concurrent requests efficiently

## Monitoring

### Health Check
```bash
curl http://localhost:8545
# Returns cache statistics
```

### Debug Logging
Enable with `DEBUG=true` to see:
- Incoming requests
- Cache hits/misses
- RPC node failures
- Failover attempts

## Production Deployment

### Docker

```dockerfile
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY . .
RUN go mod tidy && go build -o rpc-proxy

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/rpc-proxy .
EXPOSE 8545
CMD ["./rpc-proxy"]
```

### Environment Setup

```bash
# Production configuration
export ETHEREUM_RPC_NODES="https://primary.rpc.com,https://backup.rpc.com"
export PORT=8545
export CACHE_TIME=10
export DEBUG=false
```

### Load Balancing

For high availability, run multiple proxy instances behind a load balancer:

```nginx
upstream rpc_proxy {
    server proxy1:8545;
    server proxy2:8545;
    server proxy3:8545;
}

server {
    listen 80;
    location / {
        proxy_pass http://rpc_proxy;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Add tests for new functionality
4. Ensure all tests pass
5. Submit a pull request

## License

This project is licensed under the MIT License.

## Troubleshooting

### Common Issues

**No RPC nodes configured**
```
Solution: Set ETHEREUM_RPC_NODES or ETHEREUM_RPC_NODE environment variable
```

**All RPC nodes failing**
```
Check:
- RPC endpoint URLs are correct
- API keys are valid
- Network connectivity
- RPC provider status
```

**High memory usage**
```
Solutions:
- Reduce CACHE_TIME
- Monitor cache statistics
- Restart service periodically in production
```

**Slow response times**
```
Check:
- RPC provider latency
- Network connectivity
- Enable DEBUG to identify bottlenecks
```
