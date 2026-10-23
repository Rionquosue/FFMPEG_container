package service

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	Addr, WorkDir, KeysFile, Sandbox                 string
	MaxUpload, MaxOutput, StorageBudget, MaxPixels   int64
	MaxDuration                                      float64
	Concurrent, Retained, PerConsumer, MaxRecords    int
	ProcessTimeout, UploadTimeout, TTL, TombstoneTTL time.Duration
}

func DefaultConfig() Config {
	return Config{Addr: ":8080", WorkDir: "/work", KeysFile: "/run/secrets/api_keys", Sandbox: "/usr/local/bin/media-sandbox",
		MaxUpload: 256 << 20, MaxOutput: 256 << 20, StorageBudget: 768 << 20, MaxPixels: 16777216, MaxDuration: 600,
		Concurrent: 1, Retained: 2, PerConsumer: 2, MaxRecords: 1000,
		ProcessTimeout: 120 * time.Second, UploadTimeout: 120 * time.Second, TTL: 900 * time.Second, TombstoneTTL: 900 * time.Second}
}

func FromEnv() (Config, error) {
	c := DefaultConfig()
	for k, p := range map[string]*string{"HTTP_ADDR": &c.Addr, "WORK_DIR": &c.WorkDir, "API_KEYS_FILE": &c.KeysFile, "SANDBOX_PATH": &c.Sandbox} {
		if v := os.Getenv(k); v != "" {
			*p = v
		}
	}
	for k, p := range map[string]*int64{"MAX_UPLOAD_BYTES": &c.MaxUpload, "MAX_OUTPUT_BYTES": &c.MaxOutput, "STORAGE_BUDGET_BYTES": &c.StorageBudget, "MAX_PIXELS": &c.MaxPixels} {
		if v := os.Getenv(k); v != "" {
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil {
				return c, fmt.Errorf("invalid %s", k)
			}
			*p = n
		}
	}
	for k, p := range map[string]*int{"MAX_CONCURRENT_JOBS": &c.Concurrent, "MAX_RETAINED_JOBS": &c.Retained, "MAX_JOBS_PER_CONSUMER": &c.PerConsumer, "MAX_JOB_RECORDS": &c.MaxRecords} {
		if v := os.Getenv(k); v != "" {
			n, e := strconv.Atoi(v)
			if e != nil {
				return c, fmt.Errorf("invalid %s", k)
			}
			*p = n
		}
	}
	for k, p := range map[string]*time.Duration{"PROCESS_TIMEOUT_SECONDS": &c.ProcessTimeout, "UPLOAD_TIMEOUT_SECONDS": &c.UploadTimeout, "JOB_TTL_SECONDS": &c.TTL, "TOMBSTONE_TTL_SECONDS": &c.TombstoneTTL} {
		if v := os.Getenv(k); v != "" {
			n, e := strconv.ParseInt(v, 10, 32)
			if e != nil || n < 1 || n > 86400 {
				return c, fmt.Errorf("invalid %s", k)
			}
			*p = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("MAX_DURATION_SECONDS"); v != "" {
		n, e := strconv.ParseInt(v, 10, 32)
		if e != nil || n < 1 || n > 86400 {
			return c, errors.New("invalid MAX_DURATION_SECONDS")
		}
		c.MaxDuration = float64(n)
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.WorkDir) || filepath.Clean(c.WorkDir) == "/" || !filepath.IsAbs(c.Sandbox) {
		return errors.New("absolute non-root work directory and sandbox path required")
	}
	if c.MaxUpload < 1 || c.MaxOutput < 1024 || c.MaxUpload > 1<<40 || c.MaxOutput > 1<<40 || c.StorageBudget < c.MaxUpload+c.MaxOutput {
		return errors.New("invalid storage budget")
	}
	if c.Concurrent < 1 || c.Concurrent > 16 || c.Retained < c.Concurrent || c.Retained > 100 || c.PerConsumer < 1 || c.PerConsumer > c.Retained || c.MaxRecords < c.Retained || c.MaxRecords > 100000 {
		return errors.New("invalid job limits")
	}
	if c.MaxPixels < 256 || c.MaxPixels > 67108864 || c.MaxDuration < 1 || c.MaxDuration > 86400 {
		return errors.New("invalid media limits")
	}
	if c.ProcessTimeout <= 0 || c.UploadTimeout <= 0 || c.TTL <= 0 || c.TombstoneTTL <= 0 {
		return errors.New("invalid time limits")
	}
	return nil
}

type Key struct {
	Consumer string
	Digest   [32]byte
}
type keyFile struct {
	Consumers []struct {
		ID   string   `json:"id"`
		Keys []string `json:"keys"`
	} `json:"consumers"`
}

func LoadKeys(path string) ([]Key, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	var data keyFile
	if e = decodeStrict(f, &data, 65536); e != nil {
		return nil, errors.New("invalid key file")
	}
	var out []Key
	seen := map[[32]byte]bool{}
	ids := map[string]bool{}
	for _, c := range data.Consumers {
		if len(c.ID) < 1 || len(c.ID) > 64 || ids[c.ID] || len(c.Keys) < 1 || len(c.Keys) > 4 {
			return nil, errors.New("invalid consumer")
		}
		ids[c.ID] = true
		for _, s := range c.Keys {
			if len(s) < 43 || len(s) > 256 {
				return nil, errors.New("keys must contain at least 256 random bits encoded as base64url or hex")
			}
			for _, v := range s {
				if !(v >= 'a' && v <= 'z' || v >= 'A' && v <= 'Z' || v >= '0' && v <= '9' || v == '_' || v == '-') {
					return nil, errors.New("invalid key characters")
				}
			}
			d := sha256.Sum256([]byte(s))
			if seen[d] {
				return nil, errors.New("duplicate key")
			}
			seen[d] = true
			out = append(out, Key{c.ID, d})
		}
	}
	if len(out) == 0 || len(out) > 256 {
		return nil, errors.New("invalid key count")
	}
	return out, nil
}
