FROM node:24-alpine AS assets
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY scripts scripts
COPY web web
COPY templates templates
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=assets /src/static/dist static/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /notetaker ./cmd/notetaker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /notetaker /notetaker
EXPOSE 8080
ENTRYPOINT ["/notetaker"]
CMD ["serve"]
