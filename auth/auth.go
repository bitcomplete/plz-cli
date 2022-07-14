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

var ErrNoAuthCredentials = errors.New("no auth credentials")

type state struct {
	Token                 string    `json:"token"`
	ExpiresAt             time.Time `json:"expires_at"`
	RefreshToken          string    `json:"refreshToken"`
	RefreshTokenExpiresAt time.Time `json:"refreshTokenExpiresAt"`
	Type                  string    `json:"type"`
	Scope                 string    `json:"scope"`
}

type Auth struct {
	plzAPIBaseURL string
	*state
}

func New(plzAPIBaseURL string) *Auth {
	return &Auth{plzAPIBaseURL: plzAPIBaseURL}
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
	if err = browser.OpenURL(code.VerificationURI); err != nil {
		fmt.Println("Could not open a browser:", err)
		fmt.Println("Please visit this URL in your browser manually:", code.VerificationURI)
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
	// The device library doesn't return the expiry time, so we have to
	// immediately refresh the token to get the expiry time.
	state, err := loadStateFromRefreshToken(plzAPIBaseURL, accessToken.RefreshToken)
	if err != nil {
		return nil, err
	}
	return &Auth{
		plzAPIBaseURL: plzAPIBaseURL,
		state:         state,
	}, nil
}

func (a *Auth) Token() (string, error) {
	if a.state == nil {
		state, err := loadStateFromKeyRing(a.plzAPIBaseURL)
		if err != nil {
			return "", ErrNoAuthCredentials
		}
		a.state = state
	}
	// Refresh the token if it's expired or nearly expired.
	if a.state.ExpiresAt.Before(time.Now().Add(10 * time.Minute)) {
		if a.state.RefreshTokenExpiresAt.Before(time.Now().Add(10 * time.Minute)) {
			// When refresh token is expired, we have to re-auth from scratch.
			return "", ErrNoAuthCredentials
		}
		state, err := loadStateFromRefreshToken(a.plzAPIBaseURL, a.state.RefreshToken)
		if err != nil {
			return "", errors.Wrap(err, "failed to refresh auth token")
		}
		a.state = state
		err = a.SaveToKeyRing()
		if err != nil {
			return "", errors.Wrap(err, "failed to save new auth token while refreshing")
		}
	}
	return a.state.Token, nil
}

func (a *Auth) SaveToKeyRing() error {
	stateJSON, err := json.Marshal(a.state)
	if err != nil {
		return errors.WithStack(err)
	}
	return errors.WithStack(keyring.Set("plz", "authState", string(stateJSON)))
}

func loadStateFromRefreshToken(plzAPIBaseURL, refreshToken string) (*state, error) {
	params := url.Values{"refresh_token": {refreshToken}}
	refreshURL := fmt.Sprintf(
		"%s/auth/github/device/refresh?%s",
		plzAPIBaseURL,
		params.Encode(),
	)
	req, err := http.NewRequest("POST", refreshURL, nil)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	req.Header.Add("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Errorf("failed to refresh token: %s", resp.Status)
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
		return nil, errors.WithStack(err)
	}
	return &state{
		Token:                 body.AccessToken,
		ExpiresAt:             time.Now().Add(time.Duration(body.ExpiresIn) * time.Second),
		RefreshToken:          body.RefreshToken,
		RefreshTokenExpiresAt: time.Now().Add(time.Duration(body.RefreshTokenExpiresIn) * time.Second),
		Type:                  body.TokenType,
		Scope:                 body.Scope,
	}, nil
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

func loadStateFromKeyRing(plzAPIBaseURL string) (*state, error) {
	authInfoJSON, err := keyring.Get("plz", "authState")
	if err != nil {
		return nil, errors.WithStack(err)
	}
	var state state
	err = json.Unmarshal([]byte(authInfoJSON), &state)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return &state, nil
}
