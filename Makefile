PREFIX ?= ~/.local/bin

build:
	go build -o audit-loop .

install: build
	mv audit-loop $(PREFIX)/

clean:
	rm -f audit-loop
