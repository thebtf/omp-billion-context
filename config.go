package main

import (
	"crypto/sha256"
	"errors"
	"net/url"
	"os"
	"strings"
)

type config struct {
	sidecar       url.URL
	upstream      string
	alias         string
	model         string
	clientSum     [sha256.Size]byte
	upstreamToken string
}

func readConfig(getenv func(string) string) (config, error) {
	upstream, ok := httpURL(getenv("OMP_BILI_UPSTREAM_URL"))
	if !ok {
		return config{}, errors.New("OMP_BILI_UPSTREAM_URL must be an absolute HTTP(S) base without credentials, query, fragment or path escapes")
	}
	basePath := strings.TrimSuffix(upstream.Path, "/")
	for _, part := range strings.Split(basePath, "/")[1:] {
		if part == "" || part == "." || part == ".." {
			return config{}, errors.New("OMP_BILI_UPSTREAM_URL path must be canonical")
		}
	}
	if strings.HasSuffix(basePath, "/v1") {
		return config{}, errors.New("OMP_BILI_UPSTREAM_URL must omit the final /v1")
	}
	upstream.Path = basePath

	alias := getenv("OMP_BILI_MODEL_ALIAS")
	model := getenv("OMP_BILI_TARGET_MODEL")
	if alias == "" || alias != strings.TrimSpace(alias) {
		return config{}, errors.New("OMP_BILI_MODEL_ALIAS must name one exact model alias")
	}
	if model == "" || model != strings.TrimSpace(model) || model == alias {
		return config{}, errors.New("OMP_BILI_TARGET_MODEL must name a model distinct from the alias")
	}
	clientToken := getenv("OMP_BILI_CLIENT_TOKEN")
	if !bearerToken(clientToken) {
		return config{}, errors.New("OMP_BILI_CLIENT_TOKEN must be a bearer token")
	}
	upstreamToken := getenv("OMP_BILI_UPSTREAM_TOKEN")
	if upstreamToken != "" && (!bearerToken(upstreamToken) || upstreamToken == clientToken) {
		return config{}, errors.New("OMP_BILI_UPSTREAM_TOKEN must be a separate bearer token")
	}
	return config{
		upstream: upstream.String(), alias: alias, model: model,
		clientSum: sha256.Sum256([]byte(clientToken)), upstreamToken: upstreamToken,
	}, nil
}

func loadConfig() (config, error) {
	if os.Getenv("OMP_BILI_MODEL_ALIAS") == "" {
		return config{}, nil
	}
	return readConfig(os.Getenv)
}

func httpURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") ||
		u.EscapedPath() != u.Path || strings.ContainsAny(raw, "%\\") {
		return nil, false
	}
	return u, true
}

func bearerToken(token string) bool {
	if token == "" {
		return false
	}
	for i := range len(token) {
		if c := token[i]; c < 0x21 || c > 0x7e || c == ',' {
			return false
		}
	}
	return true
}
