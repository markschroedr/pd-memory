package main

import (
	"github.com/markschroedr/pd-memory/internal/cli"
	"os"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
