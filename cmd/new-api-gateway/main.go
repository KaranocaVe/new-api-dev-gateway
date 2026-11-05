package main

import (
	gateway "github.com/KaranocaVe/new-api-dev-gateway"
	"os"
)

func main() { os.Exit(gateway.Main(os.Args[1:])) }
