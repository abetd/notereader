.PHONY: build run test vet lint fmt install clean

BIN := bin/notereader

build:
	go build -o $(BIN) ./cmd/notereader

run: build
	./$(BIN) $(ARGS)

test:
	go test ./...

vet:
	go vet ./...

# golangci-lint v2 が要る（設定は .golangci.yml）
lint:
	golangci-lint run

fmt:
	golangci-lint fmt

install:
	go install ./cmd/notereader

clean:
	rm -rf bin
