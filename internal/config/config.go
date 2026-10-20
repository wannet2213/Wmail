package config

import (
	"bufio"
	"os"
	"strings"
)

// Config holds only process-level settings. Everything else (API keys,
// Cloudflare ids, limits) lives in the DB settings table so the CLI can change
// it while the server runs.
type Config struct {
	Port    string
	DBPath  string
	DataDir string
}

func LoadConfig() *Config {
	loadDotEnv(".env")
	return &Config{
		Port:    getEnv("PORT", "8080"),
		DBPath:  getEnv("DB_PATH", "./data/mail.db"),
		DataDir: getEnv("DATA_DIR", "./data/attachments"),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func loadDotEnv(filepath string) {
	file, err := os.Open(filepath)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			v = strings.Trim(v, `"'`)
			if os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
	}
}
