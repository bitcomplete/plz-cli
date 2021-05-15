package actions

import (
	"fmt"
	"net/http"

	"github.com/cli/oauth/device"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"github.com/zalando/go-keyring"
)

const gitHubAppClientID = "Iv1.39b07fd4b206e0ca"

func Auth(c *cli.Context) error {
	httpClient := http.DefaultClient
	code, err := device.RequestCode(
		httpClient,
		"https://github.com/login/device/code",
		gitHubAppClientID,
		nil,
	)
	if err != nil {
		return errors.WithStack(err)
	}
	fmt.Printf("Copy code: %s\n", code.UserCode)
	fmt.Printf("then open: %s\n", code.VerificationURI)
	accessToken, err := device.PollToken(
		httpClient,
		"https://github.com/login/oauth/access_token",
		gitHubAppClientID,
		code,
	)
	if err != nil {
		return errors.WithStack(err)
	}
	err = keyring.Set("plz", "default", accessToken.Token)
	return errors.WithStack(err)
}
