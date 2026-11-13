package main

import (
	"encoding/json"
	"fmt"
	"os"

	"reonavr/internal/testconfig"
)

func main() {
	if os.Getenv("REONVR_E2E_CREDENTIALS") != "1" {
		fail()
	}
	config, err := testconfig.Load(os.Getenv("REONVR_TEST_CONFIG"))
	if err != nil || config.ValidateLive() != nil {
		fail()
	}
	if json.NewEncoder(os.Stdout).Encode(config) != nil {
		fail()
	}
}

func fail() {
	fmt.Fprintln(os.Stderr, "E2E configuration is missing or invalid; fill in the ignored local configuration")
	os.Exit(1)
}
