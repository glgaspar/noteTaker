.PHONY: dev db build test e2e

# local RustFS credentials from docker-compose.yml
export S3_ENDPOINT ?= localhost:9000
export S3_ACCESS_KEY ?= notetaker
export S3_SECRET_KEY ?= notetaker-secret

db:
	docker compose up -d --wait

dev: db
	npm run build
	go run . serve

build:
	npm run build
	go build -o bin/notetaker .

# Go integration tests; they recreate the notetaker_test database on the compose Postgres.
test: db
	go test ./...

# Browser checks against a running server (see e2e/run.mjs for its settings).
e2e:
	cd e2e && npm install --silent && node run.mjs
