package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
)

// AuthorizeURL is where users are sent to sign in with the GitHub App.
func (c *Client) AuthorizeURL(state, redirectURI string) string {
	q := url.Values{"client_id": {c.cfg.ClientID}, "redirect_uri": {redirectURI}, "state": {state}}
	return c.cfg.WebURL + "/login/oauth/authorize?" + q.Encode()
}

type OAuthUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Name  string `json:"name"`
}

// ExchangeCode trades an OAuth code for the user's identity. The user token is
// used for this one call and then dropped; trunkcms never stores it.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string) (*OAuthUser, error) {
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_, err := c.send(ctx, "POST", c.cfg.WebURL+"/login/oauth/access_token", "", map[string]string{
		"client_id": c.cfg.ClientID, "client_secret": c.cfg.ClientSecret,
		"code": code, "redirect_uri": redirectURI,
	}, &tok, nil)
	if err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, errors.New("github login failed: " + tok.Error + " " + tok.Description)
	}
	var u OAuthUser
	if _, err := c.send(ctx, "GET", "/user", "token "+tok.AccessToken, nil, &u, nil); err != nil {
		return nil, err
	}
	return &u, nil
}

// VerifyWebhook checks the X-Hub-Signature-256 header in constant time.
func VerifyWebhook(secret, body []byte, signature string) bool {
	sig, ok := strings.CutPrefix(signature, "sha256=")
	if !ok || len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Branch is the configured content branch.
func (c *Client) Branch() string { return c.cfg.Branch }
