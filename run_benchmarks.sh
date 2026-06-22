#!/usr/bin/env bash

WORKSPACE_DIR="/home/dopleganger/Documents/Projects/ShortLink"
cd "$WORKSPACE_DIR"

echo "=================================================================="
echo " Starting ShortLink Performance Comparison Benchmark Suite"
echo "=================================================================="

# Ensure containers are running
echo "Checking Docker containers..."
docker compose up -d

# Compile the application
echo "Compiling ShortLink binary..."
go build -o shortlink .

cleanup() {
    # Clean up any lingering server process
    if [ -n "$SERVER_PID" ]; then
        echo "Cleaning up server process $SERVER_PID..."
        kill -9 $SERVER_PID 2>/dev/null || true
    fi
    # Restart cache container just in case it was left stopped
    echo "Ensuring Redis cache is running..."
    docker start shortlink_cache 2>/dev/null || true
}
trap cleanup EXIT

# -------------------------------------------------------------
# Configuration 1: WITH REDIS
# -------------------------------------------------------------
echo ""
echo "------------------------------------------------------------------"
echo " Running Scenario 1: WITH REDIS CACHING (Optimal Mode)"
echo "------------------------------------------------------------------"

# Ensure Redis is running
docker start shortlink_cache

# Run server in the background
./shortlink > server_with_redis.log 2>&1 &
SERVER_PID=$!
echo "Server started with PID: $SERVER_PID. Waiting for startup..."
sleep 3

# Run K6 benchmark
echo "Executing k6 benchmark (Scenario 1)..."
k6 run --summary-export=summary_with_redis.json benchmark.js > k6_with_redis.log 2>&1 || true

# Stop the server gracefully
echo "Stopping server gracefully..."
kill -INT $SERVER_PID
wait $SERVER_PID || true
SERVER_PID=""

# -------------------------------------------------------------
# Configuration 2: WITHOUT REDIS
# -------------------------------------------------------------
echo ""
echo "------------------------------------------------------------------"
echo " Running Scenario 2: WITHOUT REDIS CACHING (Degraded Database-Only)"
echo "------------------------------------------------------------------"

# Stop the Redis container
echo "Stopping Redis cache container..."
docker stop shortlink_cache

# Run server in the background
./shortlink > server_without_redis.log 2>&1 &
SERVER_PID=$!
echo "Server started with PID: $SERVER_PID. Waiting for startup..."
sleep 3

# Run K6 benchmark
echo "Executing k6 benchmark (Scenario 2)..."
k6 run --summary-export=summary_without_redis.json benchmark.js > k6_without_redis.log 2>&1 || true

# Stop the server gracefully
echo "Stopping server gracefully..."
kill -INT $SERVER_PID
wait $SERVER_PID || true
SERVER_PID=""

# Restart Redis
echo "Starting Redis cache container back up..."
docker start shortlink_cache

# -------------------------------------------------------------
# REPORT GENERATION
# -------------------------------------------------------------
echo ""
echo "=================================================================="
echo "                 BENCHMARK PERFORMANCE COMPARISON"
echo "=================================================================="

# Function to format time values nicely (converting float ms to ms or µs)
format_time() {
    local val=$1
    if [ "$val" = "null" ] || [ -z "$val" ] || (( $(echo "$val == 0" | bc -l) )); then
        echo "N/A"
        return
    fi
    # K6 reports trend values in milliseconds. If < 1ms, format in microseconds
    awk -v duration="$val" '
    BEGIN {
        if (duration < 1.0) {
            printf "%.2f µs", duration * 1000
        } else {
            printf "%.2f ms", duration
        }
    }'
}

# Function to parse metrics using jq
parse_metrics_json() {
    local file=$1
    if [ ! -f "$file" ]; then
        echo "0|N/A|N/A|N/A|N/A|0.00/s|0.00%"
        return
    fi
    
    local read_success=$(jq -r '.metrics.shortlink_read_success.count // 0' "$file")
    local read_avg=$(jq -r '.metrics.shortlink_read_duration.avg // 0' "$file")
    local read_med=$(jq -r '.metrics.shortlink_read_duration.med // 0' "$file")
    local read_p95=$(jq -r '.metrics.shortlink_read_duration["p(95)"] // 0' "$file")
    local write_avg=$(jq -r '.metrics.shortlink_write_duration.avg // 0' "$file")
    local total_rps=$(jq -r '.metrics.http_reqs.rate // 0' "$file")
    local error_rate=$(jq -r '.metrics.shortlink_error_rate.value // 0' "$file")
    
    # Format time durations
    local f_read_avg=$(format_time "$read_avg")
    local f_read_med=$(format_time "$read_med")
    local f_read_p95=$(format_time "$read_p95")
    local f_write_avg=$(format_time "$write_avg")
    
    # Format RPS and error rate
    local f_rps=$(awk -v rps="$total_rps" 'BEGIN { printf "%.2f/s", rps }')
    local f_err=$(awk -v err="$error_rate" 'BEGIN { printf "%.2f%%", err * 100 }')
    
    echo "$read_success|$f_read_avg|$f_read_med|$f_read_p95|$f_write_avg|$f_rps|$f_err"
}

# Parse JSON summaries
metrics_with=$(parse_metrics_json summary_with_redis.json)
metrics_without=$(parse_metrics_json summary_without_redis.json)

IFS='|' read -r w_succ w_ravg w_rmed w_rp95 w_wavg w_rps w_err <<< "$metrics_with"
IFS='|' read -r wo_succ wo_ravg wo_rmed wo_rp95 wo_wavg wo_rps wo_err <<< "$metrics_without"

echo "Configuration        | With Redis (Cached)  | Without Redis (Postgres Only)"
echo "---------------------|----------------------|------------------------------"
echo "Read Redirection (N) | $w_succ requests        | $wo_succ requests"
echo "Read Latency (Avg)   | $w_ravg              | $wo_ravg"
echo "Read Latency (Median)| $w_rmed              | $wo_rmed"
echo "Read Latency (p95)   | $w_rp95              | $wo_rp95"
echo "Write Latency (Avg)  | $w_wavg              | $wo_wavg"
echo "Overall System RPS   | $w_rps               | $wo_rps"
echo "Error Rate           | $w_err               | $wo_err"
echo "=================================================================="

# Clean up logs and summaries
rm -f server_with_redis.log server_without_redis.log k6_with_redis.log k6_without_redis.log summary_with_redis.json summary_without_redis.json
echo "Comparison run completed successfully."
