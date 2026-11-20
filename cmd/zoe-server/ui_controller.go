package main

import (
	"errors"
	"github.com/ronigooja/Zoe/internal/modelclient"
	"strconv"
)

// uiControllerFromEnv applies to the optional independent screen-display model.
func uiControllerFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (*modelclient.Client, error) {
	return modelclient.FromEnv("ZOE_UI_CONTROLLER", getenv, readFile, 30000, 500, 120000)
}

func uiControllerMaxRetries(getenv func(string) string) (int, error) {
	const key = "ZOE_UI_CONTROLLER_MAX_RETRIES"
	value := getenv(key)
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 || n > 3 {
		return 0, errors.New("invalid_configuration:" + key)
	}
	return n, nil
}
