# RPC/WebSocket Proxy Server

A high-performance RPC and WebSocket proxy server written in Go that provides caching, failover, and load balancing for Ethereum RPC and WebSocket endpoints.

## Features

- **Multiple RPC Node Support**: Configure multiple RPC endpoints with automatic failover
- **WebSocket Proxy Support**: Full WebSocket proxy with subscription handling and failover
- **Intelligent Caching**: In-memory caching with configurable TTL to reduce RPC calls
- **Failover Mechanism**: Automatically switches to healthy nodes when endpoints return 5xx errors
- **WebSocket Subscriptions**: Supports `eth_subscribe`, `eth_unsubscribe`, and subscription notifications
- **Subscription Failover**: Automatically re-establishes subscriptions when WebSocket nodes fail
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

# Multiple WebSocket endpoints (comma-separated)
export ETHEREUM_WS_NODES="wss://eth-mainnet.g.alchemy.com/v2/YOUR_KEY,wss://mainnet.infura.io/v3/YOUR_KEY"

# Or single endpoints (fallback)
export ETHEREUM_RPC_NODE="https://eth-mainnet.g.alchemy.com/v2/YOUR_KEY"
export ETHEREUM_WS_NODE="wss://eth-mainnet.g.alchemy.com/v2/YOUR_KEY"

# Optional configuration
export PORT=8545                # Server port (default: 8545)
export CACHE_TIME=5             # Cache TTL in minutes (default: 1)
export DEBUG=true               # Enable debug logging (default: false)
```

### Running the Server

```bash
go run main.go
```

The server will start on the configured port and log the number of RPC and WebSocket nodes configured.

## Usage

### HTTP RPC Requests

#### Single RPC Request

```bash
curl -X POST http://localhost:8545 \
  -H "Content-Type: application/json" \
  -d '{
    "id": 1,
    "method": "eth_blockNumber",
    "params": []
  }'
```

#### Batch RPC Request

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

### WebSocket Connections

#### Basic WebSocket Connection

```javascript
const ws = new WebSocket('ws://localhost:8545');

ws.onopen = function() {
    // Send RPC request over WebSocket
    ws.send(JSON.stringify({
        id: 1,
        method: 'eth_blockNumber',
        params: [],
        jsonrpc: '2.0'
    }));
};

ws.onmessage = function(event) {
    const response = JSON.parse(event.data);
    console.log('Response:', response);
};
```

#### WebSocket Subscriptions

```javascript
const ws = new WebSocket('ws://localhost:8545');

ws.onopen = function() {
    // Subscribe to new block headers
    ws.send(JSON.stringify({
        id: 1,
        method: 'eth_subscribe',
        params: ['newHeads'],
        jsonrpc: '2.0'
    }));
};

ws.onmessage = function(event) {
    const data = JSON.parse(event.data);

    if (data.id === 1) {
        // Subscription response
        console.log('Subscription ID:', data.result);
    } else if (data.method === 'eth_subscription') {
        // Subscription notification
        console.log('New block:', data.params);
    }
};
```

#### Unsubscribe

```javascript
// Unsubscribe using the subscription ID
ws.send(JSON.stringify({
    id: 2,
    method: 'eth_unsubscribe',
    params: ['0x1234567890abcdef'], // subscription ID from subscribe response
    jsonrpc: '2.0'
}));
```

### Cache Statistics

```bash
curl http://localhost:8545
```

## Failover Behavior

### RPC Failover

The proxy tries RPC nodes in the configured order:

1. **Network Errors**: If a node is unreachable, immediately try the next node
2. **5xx Errors**: If a node returns a 5xx HTTP status code, treat as failure and try next node
3. **Success**: Return the first successful response
4. **All Failed**: Return error response if all nodes fail

### WebSocket Failover

WebSocket connections have more complex failover behavior:

1. **Connection Failure**: If initial connection fails, try next WebSocket node
2. **Active Connection Loss**: If an active connection is lost, attempt to reconnect to next available node
3. **Subscription Re-establishment**: Active subscriptions are automatically re-established on the new connection
4. **All Nodes Failed**: Close client connection if no healthy WebSocket nodes are available

## Caching Logic

### Cached Methods
- Most read-only methods (e.g., `eth_blockNumber`, `eth_getBalance`)
- Cache key includes URL path, method, and parameters
- **WebSocket requests are not cached** due to their real-time nature

### Non-Cached Methods
- `eth_sendRawTransaction` - Transactions should not be cached
- `eth_call` - May return different results based on block state
- `eth_subscribe` / `eth_unsubscribe` - Subscription management
- Batch requests - Too complex for reliable caching
- All WebSocket requests - Real-time data requirements

### Cache Invalidation
- Time-based expiration (configurable TTL)
- No manual invalidation currently supported

## Testing

### Unit Tests

```bash
go test -v
```

### Integration Tests

Integration tests use testcontainers to spin up real Anvil (Ethereum) nodes and test failover scenarios for both RPC and WebSocket connections.

**Prerequisites:**
- Docker running
- Internet connection (to pull container images)

```bash
# Run all integration tests
./test_runner.sh

# Run specific test categories
go test -v -run TestRPCProxy     # RPC tests only
go test -v -run TestWebSocket    # WebSocket tests only
```

### Test Scenarios

#### RPC Tests
- **Failover**: First node returns 5xx, second succeeds
- **All Nodes Fail**: All nodes return 5xx errors
- **Caching**: Verify cache behavior for cacheable methods
- **Direct Methods**: Confirm non-cacheable methods bypass cache
- **Batch Requests**: Test batch request handling

#### WebSocket Tests
- **Basic Connection**: WebSocket proxy connection and message forwarding
- **Subscription Handling**: `eth_subscribe` and subscription notifications
- **WebSocket Failover**: Failover when WebSocket nodes become unavailable
- **Subscription Failover**: Re-establishment of subscriptions after failover
- **Unsubscribe**: Proper handling of `eth_unsubscribe` requests
- **All Nodes Fail**: Behavior when all WebSocket nodes are unavailable

## Configuration Reference

| Environment Variable | Description | Default | Example |
|---------------------|-------------|---------|---------|
| `ETHEREUM_RPC_NODES` | Comma-separated RPC endpoints | - | `"https://rpc1.com,https://rpc2.com"` |
| `ETHEREUM_WS_NODES` | Comma-separated WebSocket endpoints | - | `"wss://ws1.com,wss://ws2.com"` |
| `ETHEREUM_RPC_NODE` | Single RPC endpoint (fallback) | - | `"https://mainnet.infura.io/v3/KEY"` |
| `ETHEREUM_WS_NODE` | Single WebSocket endpoint (fallback) | - | `"wss://mainnet.infura.io/ws/v3/KEY"` |
| `PORT` | Server port | `8545` | `3000` |
| `CACHE_TIME` | Cache TTL in minutes | `1` | `5` |
| `DEBUG` | Enable debug logging | `false` | `true` |

## Architecture

```
Client Request
      ↓
┌─────────────┐         ┌──────────────┐
│ HTTP Client │         │ WebSocket    │
│             │         │ Client       │
└─────────────┘         └──────────────┘
      ↓                        ↓
   CORS Handler          WebSocket Upgrade
      ↓                        ↓
  Cache Check            Subscription Manager
      ↓                        ↓
 RPC Node 1 ──5xx──→ RPC Node 2   WS Node 1 ──fail──→ WS Node 2
      ↓                ↓              ↓                    ↓
   Success          Success       Success             Success
      ↓                ↓              ↓                    ↓
  Cache Store     Cache Store    Live Stream          Live Stream
      ↓                ↓              ↓                    ↓
 Client Response Client Response  Notifications     Notifications
```

## WebSocket Subscription Types

The proxy supports all standard Ethereum WebSocket subscriptions:

- **`newHeads`**: New block headers
- **`logs`**: Event logs matching filter criteria
- **`newPendingTransactions`**: New pending transactions
- **`syncing`**: Node synchronization status

Example subscription requests:

```javascript
// New block headers
{
  "id": 1,
  "method": "eth_subscribe",
  "params": ["newHeads"],
  "jsonrpc": "2.0"
}

// Event logs with filter
{
  "id": 2,
  "method": "eth_subscribe",
  "params": ["logs", {
    "address": "0xa0b86a33e6351ccb38ff4fbdefe88d9b8e6bec0c",
    "topics": ["0x..."]
  }],
  "jsonrpc": "2.0"
}
```

## Performance Considerations

- **Memory Usage**: Cache grows with unique requests; monitor in production
- **Request Latency**: First request to new endpoint may be slower due to caching
- **Failover Time**: Network timeouts add latency when nodes are down
- **Concurrent Requests**: Go's HTTP server handles concurrent requests efficiently
- **WebSocket Connections**: Each client WebSocket maintains a backend connection
- **Subscription Management**: Active subscriptions are tracked and re-established during failover

## Monitoring

### Health Check
```bash
curl http://localhost:8545
# Returns cache statistics and node status
```

### Debug Logging
Enable with `DEBUG=true` to see:
- Incoming HTTP requests
- WebSocket connection events
- Cache hits/misses
- RPC node failures
- WebSocket node failures
- Failover attempts
- Subscription management

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
export ETHEREUM_WS_NODES="wss://primary.ws.com,wss://backup.ws.com"
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

# WebSocket support requires proper upgrade headers
map $http_upgrade $connection_upgrade {
    default upgrade;
    '' close;
}

server {
    listen 80;
    location / {
        proxy_pass http://rpc_proxy;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;

        # WebSocket support
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Add tests for new functionality
4. Ensure all tests pass (both RPC and WebSocket)
5. Submit a pull request

## License

This project is licensed under the MIT License.

## Troubleshooting

### Common Issues

**No RPC/WebSocket nodes configured**
```
Solution: Set ETHEREUM_RPC_NODES/ETHEREUM_WS_NODES or fallback environment variables
```

**All RPC/WebSocket nodes failing**
```
Check:
- RPC/WebSocket endpoint URLs are correct
- API keys are valid
- Network connectivity
- Provider status
- WebSocket upgrade headers (for WebSocket connections)
```

**WebSocket subscriptions not working**
```
Check:
- WebSocket nodes support subscriptions
- Subscription type is supported by provider
- Debug logs for subscription re-establishment
```

**High memory usage**
```
Solutions:
- Reduce CACHE_TIME
- Monitor cache statistics
- Restart service periodically in production
- Monitor active WebSocket connections
```

**Slow response times**
```
Check:
- RPC/WebSocket provider latency
- Network connectivity
- Enable DEBUG to identify bottlenecks
- WebSocket connection pooling
```

**Subscription notifications stopped**
```
Check:
- Backend WebSocket connection status
- Debug logs for failover events
- Provider subscription limits
```
