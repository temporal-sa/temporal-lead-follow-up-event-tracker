#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
preview_port="${PREVIEW_PORT:-8080}"
temporal_port="${PREVIEW_TEMPORAL_PORT:-7234}"
temporal_ui_port="${PREVIEW_TEMPORAL_UI_PORT:-8234}"
preview_dir="$PWD/.local/preview"
mkdir -p "$preview_dir" bin

command -v temporal >/dev/null || { echo 'Install the Temporal CLI to run the local preview.' >&2; exit 1; }
if nc -z 127.0.0.1 "$preview_port" 2>/dev/null; then
  echo "Port $preview_port is already in use. Set PREVIEW_PORT to another port." >&2
  exit 1
fi

go build -o bin/tracker-preview ./cmd/tracker
temporal_pid=""
web_pid=""
cleanup() {
  if [[ -n "$web_pid" ]]; then kill "$web_pid" 2>/dev/null || true; wait "$web_pid" 2>/dev/null || true; fi
  if [[ -n "$temporal_pid" ]]; then kill "$temporal_pid" 2>/dev/null || true; wait "$temporal_pid" 2>/dev/null || true; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

if ! nc -z 127.0.0.1 "$temporal_port" 2>/dev/null; then
  temporal server start-dev --ip 127.0.0.1 --port "$temporal_port" --ui-port "$temporal_ui_port" \
    --db-filename "$preview_dir/temporal.db" --log-level warn >"$preview_dir/temporal.log" 2>&1 &
  temporal_pid=$!
  for ((attempt=0; attempt<100; attempt++)); do
    if nc -z 127.0.0.1 "$temporal_port" 2>/dev/null; then break; fi
    if ! kill -0 "$temporal_pid" 2>/dev/null; then cat "$preview_dir/temporal.log" >&2; exit 1; fi
    sleep 0.1
  done
fi

# Keep the preview on local Temporal, even in a shell configured for Cloud.
env -u TEMPORAL_API_KEY -u TEMPORAL_TLS -u TEMPORAL_CLIENT_CERT -u TEMPORAL_CLIENT_KEY \
  -u TEMPORAL_CA_CERT -u TEMPORAL_DEPLOYMENT_NAME -u TEMPORAL_WORKER_BUILD_ID \
  TEMPORAL_ADDRESS="127.0.0.1:$temporal_port" TEMPORAL_NAMESPACE=default \
  TEMPORAL_TASK_QUEUE=event-leads-local-preview \
  LISTEN_ADDRESS="127.0.0.1:$preview_port" PUBLIC_URL="http://localhost:$preview_port" \
  DEV_AUTH_EMAIL=developer@temporal.io bin/tracker-preview dev >"$preview_dir/app.log" 2>&1 &
web_pid=$!
for ((attempt=0; attempt<150; attempt++)); do
  if curl --fail --silent "http://localhost:$preview_port/readyz" >/dev/null; then
    echo "Local preview: http://localhost:$preview_port/admin"
    echo "Temporal UI: http://localhost:$temporal_ui_port"
    echo 'Press Ctrl+C to stop the preview. Local Temporal data is preserved.'
    wait "$web_pid"
    exit
  fi
  if ! kill -0 "$web_pid" 2>/dev/null; then cat "$preview_dir/app.log" >&2; exit 1; fi
  sleep 0.1
done
echo "Preview failed to become ready; see $preview_dir/app.log" >&2
exit 1
