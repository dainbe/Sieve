BINARY  := Sieve
MODULE  := github.com/dainbe/Sieve

.PHONY: all build test clean

all: build

build:
	go build -o $(BINARY) .

test:
	go test ./...

clean:
	rm -f $(BINARY)
