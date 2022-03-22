package actions

import (
	"github.com/bitcomplete/plz-cli/client/auth"
	"github.com/bitcomplete/plz-cli/client/deps"
	"github.com/urfave/cli/v2"
)

func Auth(c *cli.Context) error {
	deps := deps.FromContext(c.Context)
	auth, err := auth.Prompt(deps.PlzAPIBaseURL)
	if err != nil {
		return err
	}
	return auth.SaveToKeyRing()
}
