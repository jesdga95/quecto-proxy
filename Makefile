BINARY := quecto
LDFLAGS := -s -w

.PHONY: build dashboard test bench run docker clean

## build: dashboard then binary, in the order the embed needs
build: dashboard
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/quecto

## dashboard: compile the Astro build into internal/web/dist for embedding
dashboard:
	cd dashboard && npm install --no-audit --no-fund && npm run build

## test: everything, including the dashboard handler if it has been built
test:
	go test ./...

## bench: the numbers behind the performance claims in the README
bench:
	go test -bench=. -benchtime=300x -run='^$$' ./internal/...

## run: a null upstream and the proxy, for poking at it locally
run: build
	./$(BINARY)

docker:
	docker build -t quecto:latest .

clean:
	rm -f $(BINARY)
	find internal/web/dist -mindepth 1 ! -name .gitkeep -delete
