#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
image=prom/prometheus@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996
for operation in check test; do
 if [ "$operation" = check ]; then file=alerts.yml; else file=alerts.test.yml; fi
 docker run --rm --network none --entrypoint /bin/promtool \
  -v "$PWD/docker/prometheus:/rules:ro" -w /rules "$image" \
  "$operation" rules "$file"
done
# Only a synthetic token is used to validate the example's file reference.
token_file=$(mktemp)
trap 'rm -f "$token_file"' EXIT HUP INT TERM
printf '%s\n' synthetic-metrics-test-token > "$token_file"
chmod 644 "$token_file"
docker run --rm --network none --entrypoint /bin/promtool \
 -v "$PWD/docker/prometheus/prometheus.example.yml:/etc/prometheus/prometheus.yml:ro" \
 -v "$PWD/docker/prometheus/alerts.yml:/etc/prometheus/alerts.yml:ro" \
 -v "$token_file:/etc/prometheus/secrets/relaytale-metrics-token:ro" "$image" \
 check config /etc/prometheus/prometheus.yml
