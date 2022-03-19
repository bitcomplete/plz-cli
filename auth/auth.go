package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/cli/oauth/device"
	"github.com/pkg/browser"
	"github.com/pkg/errors"
	"github.com/zalando/go-keyring"
)

type state struct {
	Token                 string    `json:"token"`
	ExpiresAt             time.Time `json:"expires_at"`
	RefreshToken          string    `json:"refreshToken"`
	RefreshTokenExpiresAt time.Time `json:"refreshTokenExpiresAt"`
	Type                  string    `json:"type"`
	Scope                 string    `json:"scope"`
}

type Auth struct {
	state state
}

func (a *Auth) Token() string {
	return a.state.Token
}

func LoadFromKeyRing(plzAPIBaseURL string) (*Auth, error) {
	authInfoJSON, err := keyring.Get("plz", "authState")
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			err = nil
		}
		return nil, err
	}
	var auth Auth
	err = json.Unmarshal([]byte(authInfoJSON), &auth.state)
	if err != nil {
		return nil, err
	}
	// Refresh the token if it's expired or nearly expired.
	if auth.state.ExpiresAt.Before(time.Now().Add(10 * time.Minute)) {
		err := auth.refresh(plzAPIBaseURL)
		if err != nil {
			return nil, errors.Wrap(err, "failed to refresh auth token")
		}
	}
	return &auth, nil
}

func Prompt(plzAPIBaseURL string) (*Auth, error) {
	httpClient := http.DefaultClient
	gitHubAppClientID, err := fetchGitHubAppClientID(httpClient, plzAPIBaseURL)
	if err != nil {
		return nil, err
	}
	code, err := device.RequestCode(
		httpClient,
		"https://github.com/login/device/code",
		gitHubAppClientID,
		nil,
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	fmt.Printf("\033[33m!\033[m First copy your one-time code: \033[1m%s\033[m\n", code.UserCode)
	fmt.Println("Press Enter to open github.com in your browser...")
	fmt.Scanln()
	err = browser.OpenURL(code.VerificationURI)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	accessToken, err := device.PollToken(
		httpClient,
		"https://github.com/login/oauth/access_token",
		gitHubAppClientID,
		code,
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	auth := &Auth{
		state: state{
			Token:        accessToken.Token,
			RefreshToken: accessToken.RefreshToken,
			Type:         accessToken.Type,
			Scope:        accessToken.Scope,
		},
	}
	// The device library doesn't return the expiry time, so we have to
	// immediately refresh the token to get the expiry time.
	err = auth.refresh(plzAPIBaseURL)
	if err != nil {
		return nil, err
	}
	return auth, nil
}

func (a *Auth) SaveToKeyRing() error {
	stateJSON, err := json.Marshal(a.state)
	if err != nil {
		return err
	}
	err = keyring.Set("plz", "authState", string(stateJSON))
	return err
}

func (a *Auth) refresh(plzAPIBaseURL string) error {
	params := url.Values{"refresh_token": {a.state.RefreshToken}}
	refreshURL := fmt.Sprintf(
		"%s/auth/github/device/refresh?%s",
		plzAPIBaseURL,
		params.Encode(),
	)
	req, err := http.NewRequest("POST", refreshURL, nil)
	if err != nil {
		return errors.WithStack(err)
	}
	req.Header.Add("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.WithStack(err)
	}
	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("failed to refresh token: %s", resp.Status)
	}
	body := struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             int    `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
		Scope                 string `json:"scope"`
		TokenType             string `json:"token_type"`
	}{}
	err = json.NewDecoder(resp.Body).Decode(&body)
	if err != nil {
		return errors.WithStack(err)
	}
	a.state = state{
		Token:                 body.AccessToken,
		ExpiresAt:             time.Now().Add(time.Duration(body.ExpiresIn) * time.Second),
		RefreshToken:          body.RefreshToken,
		RefreshTokenExpiresAt: time.Now().Add(time.Duration(body.RefreshTokenExpiresIn) * time.Second),
		Type:                  body.TokenType,
		Scope:                 body.Scope,
	}
	return nil
}

func fetchGitHubAppClientID(client *http.Client, plzAPIBaseURL string) (string, error) {
	resp, err := client.Get(plzAPIBaseURL + "/clientid")
	if err != nil {
		return "", errors.WithStack(err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.Errorf("failed to fetch app client ID: %s", resp.Status)
	}
	clientIDBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return string(clientIDBytes), nil
}
