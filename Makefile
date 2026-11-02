.PHONY: dev db build

# local RustFS credentials from docker-compose.yml
export S3_ENDPOINT ?= localhost:9000
export S3_ACCESS_KEY ?= notetaker
export S3_SECRET_KEY ?= notetaker-secret

db:
	docker compose up -d

dev: db
	npm run build
	go run . serve

build:
	npm run build
	go build -o bin/notetaker .
