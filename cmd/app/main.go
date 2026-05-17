package main

import (
	"fmt"
	"os"

	"github.com/Borislavv/polymarket-watchtower/internal/app"
)

func main() {
	a, err := app.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "watchtower: init failed: %v\n", err)
		os.Exit(1)
	}
	if err := a.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "watchtower: exited with error: %v\n", err)
		os.Exit(1)
	}
}
