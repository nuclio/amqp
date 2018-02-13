PACKAGE := github.com/nuclio/amqp
FUZZ_DIR := ./fuzz

all: test

fuzzconn:
	go-fuzz-build -o $(FUZZ_DIR)/conn.zip -func FuzzConn $(PACKAGE)
	go-fuzz -bin $(FUZZ_DIR)/conn.zip -workdir $(FUZZ_DIR)/conn

fuzzmarshal:
	go-fuzz-build -o $(FUZZ_DIR)/marshal.zip -func FuzzUnmarshal $(PACKAGE)
	go-fuzz -bin $(FUZZ_DIR)/marshal.zip -workdir $(FUZZ_DIR)/marshal

fuzzclean:
	rm -f $(FUZZ_DIR)/**/{crashers,suppressions}/*
	rm -f $(FUZZ_DIR)/*.zip

test:
	TEST_CORPUS=1 go test -tags gofuzz -race -run=Corpus
	go test -tags gofuzz -v -race ./...

integration:
	go test -tags "integration pkgerrors" -v -race ./...

coverage:
	TEST_CORPUS=1 go test -tags "integration gofuzz" -cover -coverprofile=cover.out -v
