#!/bin/bash

set -e

echo "Setting up Go module..."
go mod tidy

echo "Running integration tests..."
go test -v -timeout 300s -run Test

echo "Running specific failover test..."
go test -v -timeout 60s -run TestRPCProxyFailover

echo "Running caching test..."
go test -v -timeout 60s -run TestRPCProxyCaching

echo "Running all nodes fail test..."
go test -v -timeout 60s -run TestRPCProxyAllNodesFail

echo "All tests completed!"
