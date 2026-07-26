package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Credentials for the shared tier come from Hanzo KMS. Two endpoints, the same
// pair a CI job uses: exchange a machine identity for a bearer token, then read
// the secret. Nothing is written to disk and nothing is cached between runs.
//
//	POST {kms}/v1/kms/auth/login   {"clientId","clientSecret"} -> {"accessToken"}
//	GET  {kms}/v1/kms/orgs/{org}/secrets/{path}/{key}?env={env} -> {"secret":{"value"}}

type kms struct {
	addr   string
	org    string
	env    string
	id     string
	secret string
	http   *http.Client
}

func (k kms) token(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{"clientId": k.id, "clientSecret": k.secret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.addr+"/v1/kms/auth/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := k.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("kms login: %s", res.Status)
	}
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("kms login: no token")
	}
	return out.AccessToken, nil
}

func (k kms) get(ctx context.Context, token, path, key string) (string, error) {
	url := fmt.Sprintf("%s/v1/kms/orgs/%s/secrets/%s/%s?env=%s",
		k.addr, k.org, strings.Trim(path, "/"), key, k.env)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	res, err := k.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("kms get %s/%s: %s", path, key, res.Status)
	}
	var out struct {
		Secret struct {
			Value string `json:"value"`
		} `json:"secret"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	if out.Secret.Value != "" {
		return out.Secret.Value, nil
	}
	if out.Value == "" {
		return "", fmt.Errorf("kms get %s/%s: empty", path, key)
	}
	return out.Value, nil
}

// pair reads two keys from one secret path in a single authenticated session.
func (k kms) pair(ctx context.Context, path, a, b string) (string, string, error) {
	tok, err := k.token(ctx)
	if err != nil {
		return "", "", err
	}
	av, err := k.get(ctx, tok, path, a)
	if err != nil {
		return "", "", err
	}
	bv, err := k.get(ctx, tok, path, b)
	if err != nil {
		return "", "", err
	}
	return av, bv, nil
}
