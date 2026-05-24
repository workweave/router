#!/bin/bash
# Custom entrypoint for the Pub/Sub emulator used by docker-compose. Boots the
# emulator and pre-creates the topic the router publishes invalidation
# messages on. Subscriptions are NOT created here — the router creates its own
# per-replica subscription on startup (so every replica receives every
# invalidation instead of one replica winning a load-balanced delivery).

set -e

PROJECT_ID="${PUBSUB_PROJECT_ID:-router-local}"
PORT="${PUBSUB_PORT:-8085}"
HOST_PORT="0.0.0.0:${PORT}"
INVALIDATION_TOPIC="${PUBSUB_TOPIC_ROUTER_INVALIDATION:-router-installation-invalidate}"

gcloud beta emulators pubsub start --host-port="${HOST_PORT}" --project="${PROJECT_ID}" &
EMULATOR_PID=$!

echo "Waiting for Pub/Sub emulator to start..."
for i in {1..30}; do
    if curl -s "http://localhost:${PORT}" > /dev/null 2>&1; then
        echo "Pub/Sub emulator is ready!"
        break
    fi
    sleep 1
done

echo "Creating topic: ${INVALIDATION_TOPIC}"
curl -s -X PUT "http://localhost:${PORT}/v1/projects/${PROJECT_ID}/topics/${INVALIDATION_TOPIC}" > /dev/null || true

echo "Pub/Sub emulator initialized."

wait $EMULATOR_PID
