package main

import (
	"os"
	"strings"
	"time"

	"github.com/ronigooja/Zoe/internal/logging"
	"github.com/ronigooja/Zoe/internal/memoryapi"
)

func memoryAPIFromEnv() *memoryapi.Client {
	var missing []string
	for _, name := range []string{"ZOE_MEMORY_API_URL", "ZOE_MEMORY_API_TOKEN_FILE"} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		logging.Infof("memory status=disabled reason=not_configured missing=%s", strings.Join(missing, ","))
		return nil
	}
	client, err := memoryapi.New(os.Getenv("ZOE_MEMORY_API_URL"), os.Getenv("ZOE_MEMORY_API_TOKEN_FILE"), 60*time.Second)
	if err != nil {
		logging.Errorf("memory status=disabled reason=invalid_configuration")
		return nil
	}
	return client
}
