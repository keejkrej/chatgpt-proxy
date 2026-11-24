package main

import (
	"encoding/json"
	"errors"
	"io"
)

type credentials struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	IDToken     string `json:"id_token"`
}

// Read on each request so a renewed login or logout takes effect without a restart.
// This process never reads refresh tokens into a struct or writes the file.
func readCredentials(path string) (credentials, error) {
	file, err := openAuth(path)
	if err != nil {
		return credentials{}, err
	}
	defer file.Close()

	const limit = 64 << 10
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return credentials{}, err
	}
	var auth struct {
		Mode   string      `json:"auth_mode"`
		Tokens credentials `json:"tokens"`
	}
	if len(body) > limit || json.Unmarshal(body, &auth) != nil {
		return credentials{}, errors.New("invalid Codex auth file")
	}
	if auth.Mode != "" && auth.Mode != "chatgpt" || auth.Tokens.AccessToken == "" || auth.Tokens.AccountID == "" {
		return credentials{}, errors.New("Codex auth file does not contain a ChatGPT login")
	}
	return auth.Tokens, nil
}
