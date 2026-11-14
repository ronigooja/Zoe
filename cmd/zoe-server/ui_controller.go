package main

import "github.com/ronigooja/Zoe/internal/modelclient"

// uiControllerFromEnv applies to the optional independent screen-display model.
func uiControllerFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (*modelclient.Client, error) {
	return modelclient.FromEnv("ZOE_UI_CONTROLLER", getenv, readFile, 60000, 500, 120000)
}
