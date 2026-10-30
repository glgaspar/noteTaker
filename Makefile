.PHONY: dev db build

db:
	docker compose up -d db

dev: db
	npm run build
	go run . serve

build:
	npm run build
	go build -o bin/notetaker .
