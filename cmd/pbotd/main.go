package main

import (
	"github.com/decfrr/pbotd/internal/cli"
	"os"
)

func main() { os.Exit(cli.Run(os.Args[0], os.Args[1:], os.Stdout, os.Stderr)) }
