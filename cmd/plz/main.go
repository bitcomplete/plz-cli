package main

import (
	"io/ioutil"
	"log"
	"os"

	"github.com/bitcomplete/plz-cli/client/actions"
	"github.com/bitcomplete/plz-cli/client/auth"
	"github.com/bitcomplete/plz-cli/client/deps"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
)

var Version = "dev"

func main() {
	app := &cli.App{
		Version: Version,
		Usage:   "plz.review command-line companion",
		Commands: []*cli.Command{
			{
				Name:   "auth",
				Usage:  "authorize GitHub access",
				Action: actions.Auth,
			},
			{
				Name:   "review",
				Usage:  "start a review",
				Action: actions.Review,
				Flags: []cli.Flag{
					&cli.StringSliceFlag{
						Name:    "reviewer",
						Aliases: []string{"r"},
						Usage:   "add reviewer by GitHub username",
					},
				},
			},
			{
				Name:   "switch",
				Usage:  "switch to a review branch",
				Action: actions.Switch,
			},
			{
				Name:   "sync",
				Usage:  "update local review branches",
				Action: actions.Sync,
			},
			{
				Name:   "status",
				Usage:  "list local review status",
				Action: actions.Status,
			},
		},
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "verbose",
				Usage: "show verbose debug output",
			},
			&cli.StringFlag{
				Name:  "plz-api-base-url",
				Value: "https://api.plz.review",
				Usage: "point to a different plz server",
			},
		},
		Before: func(c *cli.Context) error {
			debugWriter := ioutil.Discard
			if c.Bool("verbose") {
				debugWriter = os.Stdout
			}
			plzAPIBaseURL := c.String("plz-api-base-url")
			c.Context = deps.ContextWithDeps(c.Context, &deps.Deps{
				ErrorLog:      log.New(os.Stderr, "", 0),
				InfoLog:       log.New(os.Stdout, "", 0),
				DebugLog:      log.New(debugWriter, "[debug] ", log.Ldate|log.Lmicroseconds),
				PlzAPIBaseURL: plzAPIBaseURL,
				Auth:          auth.New(plzAPIBaseURL),
			})
			return nil
		},
		ExitErrHandler: func(c *cli.Context, err error) {
			deps := deps.FromContext(c.Context)
			if err != nil {
				if errors.Is(err, auth.ErrNoAuthCredentials) {
					deps.ErrorLog.Println("no auth credentials, run plz auth")
				} else {
					deps.ErrorLog.Println(err.Error())
					var stackTracer interface {
						StackTrace() errors.StackTrace
					}
					if errors.As(err, &stackTracer) {
						deps.DebugLog.Printf("%+v", stackTracer.StackTrace())
					}
				}
				os.Exit(1)
			}
		},
	}
	_ = app.Run(os.Args)
}
