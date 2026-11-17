package main

import (
	"errors"
	"strconv"
	"strings"
)

func uiControllerMaxAttempts(getenv func(string) string) (int, error) {
	raw := strings.TrimSpace(getenv("ZOE_UI_CONTROLLER_MAX_ATTEMPTS"))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n == int(^uint(0)>>1) {
		return 0, errors.New("ZOE_UI_CONTROLLER_MAX_ATTEMPTS must be a nonnegative integer less than the platform maximum")
	}
	return n, nil
}
