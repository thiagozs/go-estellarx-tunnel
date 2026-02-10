MODULE   := github.com/thiagozs/go-estellarx-tunnel
BIN_DIR  := bin

.PHONY: all build server client clean fmt vet tidy cross-linux

all: build

build: server client

server:
	go build -o $(BIN_DIR)/tunnel-server ./server

client:
	go build -o $(BIN_DIR)/tunnel-client ./client

cross-linux:
	GOOS=linux GOARCH=amd64 go build -o $(BIN_DIR)/tunnel-server-linux-amd64 ./server
	GOOS=linux GOARCH=amd64 go build -o $(BIN_DIR)/tunnel-client-linux-amd64 ./client

clean:
	rm -rf $(BIN_DIR)

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy

run-server:
	go run ./server serve -p 8080 -d localhost

run-client:
	go run ./client expose 3000 -s ws://localhost:8080/_tunnel/ws -d myapp
